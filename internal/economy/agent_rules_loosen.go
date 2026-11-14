package economy

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/lens/internal/passkey"
)

// agent_rules_loosen.go — B55.13: LOOSENING AN AGENT'S RULES TAKES THE SAME SIGNATURE AS RESUMING IT.
//
// Tightening is instant, loosening is signed, as pausing and resuming are (B41.108). A change to an agent's rules —
// new rules, a template, a rollback — is made in a transaction holding the agent's rules lock and judged by the
// rules it puts in force against those in force before it. When it loosens any rule and the workspace has a passkey or
// device key, it commits only with an assertion by one of them over a challenge naming the agent and a hash of exactly
// those rules, spent in the same transaction. A boost always loosens, and is signed over a hash of itself; ending one
// never does. RulesChallenge makes the change, reads it and rolls it back, so it answers what a save would do.

// ErrRulesUnsigned refuses a looser change with no assertion in a workspace that has a passkey or device key.
var ErrRulesUnsigned = errors.New("loosening an agent's rules is signed with your passkey or your phone: sign this change's challenge")

// RulesProposal is one change to an agent's rules: exactly one of new rules, a version to roll back to, a template
// to apply, or a boost.
type RulesProposal struct {
	Rules      *AgentRules     `json:"rules"`
	RollbackTo int             `json:"rollback_to"`
	Template   string          `json:"template"`
	Boost      *AgentRuleBoost `json:"boost"`
}

// RulesJudgement is what a proposal would do: whether it loosens the rules in force, every rule it changes in the
// words rules history uses, and — when it loosens them — the challenge a signature over it answers.
type RulesJudgement struct {
	Looser           bool     `json:"looser"`
	Changes          []string `json:"changes"`
	Challenge        string   `json:"challenge,omitempty"`
	AllowCredentials []string `json:"allow_credentials"`
}

// ruleLabels name the limits as the Rules card and rules history do.
var ruleLabels = map[string]string{
	"max_per_request_ulxc": "Limit per request",
	"hourly_limit_ulxc":    "Hourly limit",
	"daily_limit_ulxc":     "Daily limit",
	"weekly_limit_ulxc":    "Weekly limit",
	"monthly_limit_ulxc":   "Monthly limit",
	"approval_above_ulxc":  "Ask a person above",
	"requests_per_minute":  "Requests a minute",
	"max_commitment_ulxc":  "Most it may commit",
	"fx_daily_limit_ulxc":  "Daily conversion maximum",
}

type ruleChange struct {
	words  string
	looser bool
}

// limitLoosened: a limit raised or cleared. A zero is no limit, so setting one where there was none tightens.
func limitLoosened(was, now int64) bool { return was > 0 && (now == 0 || now > was) }

// listLoosened: an allowed list that named any now names one more, or none (which allows all).
func listLoosened(was, now []string) bool {
	if len(was) == 0 {
		return false
	}
	return len(now) == 0 || slices.ContainsFunc(now, func(id string) bool { return !slices.Contains(was, id) })
}

// activeMinutes are the minutes of the day r's active hours let it spend; nil is all day.
func activeMinutes(r AgentRules) []bool {
	if r.ActiveFrom == "" {
		return nil
	}
	from, _ := clockMinutes(r.ActiveFrom)
	until, _ := clockMinutes(r.ActiveUntil)
	in := make([]bool, 24*60)
	for m := from; m != until; m = (m + 1) % (24 * 60) {
		in[m] = true
	}
	return in
}

// hoursLoosened: the time zone changed, or the new active hours do not sit inside the old ones.
func hoursLoosened(was, now AgentRules) bool {
	if cmp.Or(was.Timezone, "UTC") != cmp.Or(now.Timezone, "UTC") {
		return true
	}
	old, next := activeMinutes(was), activeMinutes(now)
	if old == nil {
		return false
	}
	if next == nil {
		return true
	}
	for m, in := range next {
		if in && !old[m] {
			return true
		}
	}
	return false
}

// rulesChanges is every rule that differs between was and now, in rules history's words, each with whether it loosens.
func rulesChanges(was, now AgentRules) []ruleChange {
	var out []ruleChange
	add := func(rule, from, to string, looser bool) {
		if from != to {
			out = append(out, ruleChange{fmt.Sprintf("%s: was %s, now %s", rule, from, to), looser})
		}
	}
	amount := func(v int64) string {
		if v > 0 {
			return LXCMoney(v)
		}
		return "none"
	}
	for _, l := range []struct {
		rule     string
		was, now int64
	}{
		{"max_per_request_ulxc", was.MaxPerRequestULXC, now.MaxPerRequestULXC},
		{"hourly_limit_ulxc", limitOf(was.HourlyLimitULXC), limitOf(now.HourlyLimitULXC)},
		{"daily_limit_ulxc", was.DailyLimitULXC, now.DailyLimitULXC},
		{"weekly_limit_ulxc", limitOf(was.WeeklyLimitULXC), limitOf(now.WeeklyLimitULXC)},
		{"monthly_limit_ulxc", was.MonthlyLimitULXC, now.MonthlyLimitULXC},
		{"approval_above_ulxc", was.ApprovalAboveULXC, now.ApprovalAboveULXC},
		{"requests_per_minute", limitOf(was.RequestsPerMinute), limitOf(now.RequestsPerMinute)},
		{"max_commitment_ulxc", limitOf(was.MaxCommitmentULXC), limitOf(now.MaxCommitmentULXC)},
		{"fx_daily_limit_ulxc", limitOf(was.FXDailyLimitULXC), limitOf(now.FXDailyLimitULXC)},
	} {
		text := amount
		if l.rule == "requests_per_minute" {
			text = func(v int64) string {
				if v > 0 {
					return strconv.FormatInt(v, 10)
				}
				return "none"
			}
		}
		add(ruleLabels[l.rule], text(l.was), text(l.now), limitLoosened(l.was, l.now))
	}
	for _, c := range []struct {
		rule     func(string) string
		was, now map[string]int64
	}{
		{func(m string) string { return "Daily limit on " + m }, was.ModelDailyLimitsULXC, now.ModelDailyLimitsULXC},
		{func(id string) string { return "Daily limit to " + id }, was.PayeeDailyLimitsULXC, now.PayeeDailyLimitsULXC},
		{func(id string) string { return "Monthly limit to " + id }, was.PayeeMonthlyLimitsULXC, now.PayeeMonthlyLimitsULXC},
	} {
		for _, key := range slices.Sorted(maps.Keys(mergedKeys(c.was, c.now))) {
			add(c.rule(key), amount(c.was[key]), amount(c.now[key]), limitLoosened(c.was[key], c.now[key]))
		}
	}
	for _, l := range []struct {
		rule, none string
		was, now   []string
		blocked    bool
	}{
		{"Allowed models", "any", was.AllowedModels, now.AllowedModels, false},
		{"Allowed providers", "any", was.AllowedProviders, now.AllowedProviders, false},
		{"Allowed listings", "any", was.AllowedListings, now.AllowedListings, false},
		{"May pay only", "anyone", was.AllowedPayees, now.AllowedPayees, false},
		{"Licences it may hold", "any", was.AllowedLicences, now.AllowedLicences, false},
		{"May convert between", "any", was.AllowedCurrencies, now.AllowedCurrencies, false},
		{"May never pay", "nobody", was.BlockedPayees, now.BlockedPayees, true},
	} {
		text := func(ids []string) string {
			if len(ids) == 0 {
				return l.none
			}
			return strings.Join(slices.Sorted(slices.Values(ids)), ", ")
		}
		looser := listLoosened(l.was, l.now)
		if l.blocked {
			looser = slices.ContainsFunc(l.was, func(id string) bool { return !slices.Contains(l.now, id) })
		}
		add(l.rule, text(l.was), text(l.now), looser)
	}
	allowed := func(a PayeeAllowance) string {
		if a.ULXC <= 0 { // none, or 0, which allows nothing
			return "none"
		}
		return fmt.Sprintf("up to %s a %s", LXCMoney(a.ULXC), a.Per)
	}
	for _, id := range slices.Sorted(maps.Keys(mergedKeys(was.PayeeAllowances, now.PayeeAllowances))) {
		b, n := was.PayeeAllowances[id], now.PayeeAllowances[id]
		// B54.29: an amount a month is never more than the same amount a day, so only a month made a day loosens it.
		add("Always allow "+id, allowed(b), allowed(n), n.ULXC > b.ULXC || (n.ULXC > 0 && b.Per == "month" && n.Per == "day"))
	}
	hours := hoursLoosened(was, now)
	add("Works from", cmp.Or(was.ActiveFrom, "any time"), cmp.Or(now.ActiveFrom, "any time"), hours)
	add("Works until", cmp.Or(was.ActiveUntil, "any time"), cmp.Or(now.ActiveUntil, "any time"), hours)
	add("Time zone", cmp.Or(was.Timezone, "UTC"), cmp.Or(now.Timezone, "UTC"), hours)
	onOff := map[bool]string{true: "on", false: "off"}
	add("Pause on unusual spend", onOff[was.PauseOnUnusualSpend], onOff[now.PauseOnUnusualSpend], was.PauseOnUnusualSpend)
	yesNo := map[bool]string{true: "yes", false: "no"}
	wasSub, nowSub := was.MaySubscribe != nil && *was.MaySubscribe, now.MaySubscribe != nil && *now.MaySubscribe
	add("May subscribe", yesNo[wasSub], yesNo[nowSub], nowSub)
	wasInstant, nowInstant := was.MayInstantTransfer != nil && *was.MayInstantTransfer, now.MayInstantTransfer != nil && *now.MayInstantTransfer
	add("Can use Instant transfer", yesNo[wasInstant], yesNo[nowInstant], nowInstant)
	return out
}

func mergedKeys[V any](a, b map[string]V) map[string]bool {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	return keys
}

// hashPurpose names a signed change to agentID: kind and a hash of exactly what it puts in force.
func hashPurpose(kind, agentID string, v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return kind + ":" + agentID + ":" + hex.EncodeToString(sum[:]), nil
}

func boostPurpose(agentID string, b AgentRuleBoost) (string, error) {
	return hashPurpose("boost", agentID, []any{b.Rule, b.Value, b.Until.UTC().Format(time.RFC3339Nano)})
}

// proposeRulesTx makes p — not a boost — in tx, holding the agent's rules locked, and judges what it puts in force.
func (s *DualTokenStore) proposeRulesTx(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, p RulesProposal) (AgentRules, RulesJudgement, string, error) {
	j := RulesJudgement{Changes: []string{}, AllowCredentials: []string{}}
	if err := agentExists(ctx, tx, workspaceID, agentID); err != nil {
		return AgentRules{}, j, "", err
	}
	// Held until tx ends, so no other change lands between the rules judged and the rules saved — even an agent's first.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('agent_rules:' || $1, 0))`, agentID); err != nil {
		return AgentRules{}, j, "", fmt.Errorf("economy: agent rules: %w", err)
	}
	was, err := scanAgentRules(tx.QueryRow(ctx, `SELECT `+agentRulesColumns+` FROM agent_rules WHERE agent_id = $1`, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		was, err = AgentRules{}, nil
	}
	if err != nil {
		return was, j, "", fmt.Errorf("economy: agent rules: %w", err)
	}
	var now AgentRules
	switch {
	case p.Rules != nil:
		now, err = s.setAgentRulesTx(ctx, tx, workspaceID, agentID, *p.Rules)
	case p.Template != "":
		tmpl, ok := RuleTemplateByID(p.Template)
		if !ok {
			return was, j, "", fmt.Errorf("%w: there is no rule template %q", ErrAgentRule, p.Template)
		}
		now, err = s.setAgentRulesTx(ctx, tx, workspaceID, agentID, tmpl.Rules)
	case p.RollbackTo > 0:
		now, err = s.rollbackAgentRulesTx(ctx, tx, workspaceID, agentID, p.RollbackTo)
	default:
		return was, j, "", fmt.Errorf("%w: name the change: rules, rollback_to, template or boost", ErrAgentRule)
	}
	if err != nil {
		return now, j, "", err
	}
	// The rules as saved, read back, so a challenge and its save hash the same rules.
	if now, err = scanAgentRules(tx.QueryRow(ctx, `SELECT `+agentRulesColumns+` FROM agent_rules WHERE agent_id = $1`, agentID)); err != nil {
		return now, j, "", fmt.Errorf("economy: agent rules: %w", err)
	}
	purpose, err := hashPurpose("rules", agentID, now)
	if err != nil {
		return now, j, "", err
	}
	// Judged as saved and with the boosts in force laid over both: a limit raised under a boost is raised once it ends,
	// and a limit put back where a boost was raised from brings the boost back.
	changes := rulesChanges(was, now)
	at := time.Now()
	boosts, err := agentBoosts(ctx, tx, agentID, at)
	if err != nil {
		return now, j, "", err
	}
	was.applyBoosts(boosts)
	inForce, err := agentRulesInForce(ctx, tx, agentID, at)
	if err != nil {
		return now, j, "", fmt.Errorf("economy: agent rules: %w", err)
	}
	for _, c := range append(changes, rulesChanges(was, inForce)...) {
		if !slices.Contains(j.Changes, c.words) {
			j.Changes = append(j.Changes, c.words)
		}
		j.Looser = j.Looser || c.looser
	}
	return now, j, purpose, nil
}

// RulesChallenge answers what p would do to the agent's rules and, when it loosens them, issues the challenge a
// signature over it answers, and the workspace's passkeys that may sign it. Nothing is saved.
func (s *DualTokenStore) RulesChallenge(ctx context.Context, workspaceID, agentID string, p RulesProposal) (RulesJudgement, error) {
	j := RulesJudgement{Changes: []string{}, AllowCredentials: []string{}}
	var purpose string
	if p.Boost != nil {
		r, err := s.GetAgentRules(ctx, workspaceID, agentID)
		if err != nil {
			return j, err
		}
		was := int64(0)
		if field, ok := boostableRules[p.Boost.Rule]; ok && field(&r) != nil {
			was = *field(&r)
		}
		j.Looser = true
		j.Changes = append(j.Changes, fmt.Sprintf("%s: was %s, now %s until %s", cmp.Or(ruleLabels[p.Boost.Rule], p.Boost.Rule),
			ruleAmount(p.Boost.Rule, was), ruleAmount(p.Boost.Rule, p.Boost.Value), p.Boost.Until.UTC().Format(time.RFC3339)))
		if purpose, err = boostPurpose(agentID, *p.Boost); err != nil {
			return j, err
		}
	} else {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return j, err
		}
		_, j, purpose, err = s.proposeRulesTx(ctx, tx, workspaceID, agentID, p)
		_ = tx.Rollback(ctx)
		if err != nil {
			return j, err
		}
	}
	if !j.Looser {
		return j, nil
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return j, err
	}
	for _, k := range keys {
		j.AllowCredentials = append(j.AllowCredentials, k.CredentialID)
	}
	j.Challenge, err = s.newChallenge(ctx, workspaceID, purpose)
	return j, err
}

// ChangeAgentRules makes p — not a boost — and returns the rules now in force. When it loosens them and the workspace
// has a passkey or device key, only with a's assertion over RulesChallenge's challenge for exactly p, spent in the
// same transaction.
func (s *DualTokenStore) ChangeAgentRules(ctx context.Context, workspaceID, agentID string, p RulesProposal, a *passkey.Assertion) (AgentRules, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentRules{}, fmt.Errorf("economy: set agent rules: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now, j, purpose, err := s.proposeRulesTx(ctx, tx, workspaceID, agentID, p)
	if err != nil {
		return now, err
	}
	if j.Looser {
		if err := s.authorizeSignedTx(ctx, tx, workspaceID, purpose, a, ErrRulesUnsigned); err != nil {
			return now, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return now, fmt.Errorf("economy: set agent rules: %w", err)
	}
	return now, nil
}

// BoostAgentRuleSigned is BoostAgentRule once the workspace's passkey or device key has signed exactly b, when it
// has one: a boost always loosens the rules. Judged and saved in the transaction that spends the signature, holding
// the agent's rules lock, so a boost the rules refuse leaves its signature unspent (B54.29).
func (s *DualTokenStore) BoostAgentRuleSigned(ctx context.Context, workspaceID, agentID string, b AgentRuleBoost, a *passkey.Assertion) (AgentRuleBoost, error) {
	purpose, err := boostPurpose(agentID, b)
	if err != nil {
		return b, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return b, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('agent_rules:' || $1, 0))`, agentID); err != nil {
		return b, fmt.Errorf("economy: boost agent rule: %w", err)
	}
	if b, err = s.boostAgentRule(ctx, tx, workspaceID, agentID, b); err != nil {
		return b, err
	}
	if err := s.authorizeSignedTx(ctx, tx, workspaceID, purpose, a, ErrRulesUnsigned); err != nil {
		return b, err
	}
	return b, tx.Commit(ctx)
}
