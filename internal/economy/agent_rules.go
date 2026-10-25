package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // active hours are read in any IANA zone; the runtime image carries no zoneinfo

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// agent_rules.go — B19.2: SPENDING RULES PER AGENT, ENFORCED BEFORE THE PROVIDER IS CALLED.
//
// The rules are judged inside agentMovement — the hold or the debit of an agent's key, under the
// agent's row lock — so they bind every seam that spends an agent's balance (the reservation and the
// immediate debit, buffered and streamed), and concurrent requests cannot together pass a daily or
// monthly limit. What the rules need to know about the request beyond its amount — model, provider,
// fingerprint — rides the context (WithAgentRequest). A request above the approval amount files an
// approval (migration 0141) and is refused until the workspace's owner approves it; the approved
// request is then let through once.

// ErrAgentRule wraps every refusal by an agent's rules except the approval amount's.
var ErrAgentRule = errors.New("economy: the agent's spending rules refuse this request")

// ErrAgentRequestRate is the requests-per-minute rule's refusal (B28.302): an ErrAgentRule, which the proxy answers
// 429 rather than 403, because the same request goes through once the minute has room again.
var ErrAgentRequestRate = fmt.Errorf("%w", ErrAgentRule)

// ErrApprovalRequired is the approval amount's refusal; *ApprovalNeededError carries the approval.
var ErrApprovalRequired = errors.New("economy: this request needs a person's approval")

// ErrApprovalNotFound: no such open approval in this workspace.
var ErrApprovalNotFound = errors.New("economy: no such pending approval in this workspace")

// AgentRules are one agent's spending rules. A zero amount, an empty list or an empty window is no rule.
type AgentRules struct {
	MaxPerRequestULXC int64 `json:"max_per_request_ulxc"`
	// HourlyLimitULXC and WeeklyLimitULXC (B28.300) count the clock hour and the week from Monday. Read, they
	// are never nil; nil (absent from the JSON) saves the rules without changing them, like AllowedListings.
	HourlyLimitULXC   *int64   `json:"hourly_limit_ulxc"`
	DailyLimitULXC    int64    `json:"daily_limit_ulxc"`
	WeeklyLimitULXC   *int64   `json:"weekly_limit_ulxc"`
	MonthlyLimitULXC  int64    `json:"monthly_limit_ulxc"`
	ApprovalAboveULXC int64    `json:"approval_above_ulxc"`
	AllowedModels     []string `json:"allowed_models"`
	AllowedProviders  []string `json:"allowed_providers"`
	AllowedListings   []string `json:"allowed_listings"` // marketplace listings it may use (B19.14); empty allows any
	ActiveFrom        string   `json:"active_from"`      // "HH:MM" in Timezone; with ActiveUntil, the hours it may spend
	ActiveUntil       string   `json:"active_until"`     // exclusive; earlier than ActiveFrom means the window crosses midnight
	Timezone          string   `json:"timezone"`         // IANA name; UTC when empty. Days and months are counted in it too
	// PauseOnUnusualSpend pauses the agent when its spend raises an unusual-spend alert (B19.6).
	PauseOnUnusualSpend bool `json:"pause_on_unusual_spend"`
	// ModelDailyLimitsULXC (B28.301) caps what the agent spends on one model, named as it asks for it, in a day.
	// Read, it is never nil; nil (absent from the JSON) saves the rules without changing it. Saved, it replaces
	// the stored caps whole: a model left out, or capped at zero, has no cap.
	ModelDailyLimitsULXC map[string]int64 `json:"model_daily_limits_ulxc"`
	// RequestsPerMinute (B28.302) caps the questions the agent asks in any sixty seconds. Read, it is never nil;
	// nil (absent from the JSON) saves the rules without changing it.
	RequestsPerMinute *int64 `json:"requests_per_minute"`
	// AllowedPayees and BlockedPayees (B28.303) name, by id, who the agent may pay and who it may not: an agent
	// (agt_…), a listing (lst_…), a company (its workspace id: its agents and listings too) or a card merchant. Once
	// AllowedPayees names any, a payee it does not name is refused. Read, they are never nil; nil (absent from the
	// JSON) saves the rules without changing them.
	AllowedPayees []string `json:"allowed_payees"`
	BlockedPayees []string `json:"blocked_payees"`
	// PayeeDailyLimitsULXC (B28.304) caps what the agent pays one payee, named by id as the payee lists name it, in
	// a day. Read, it is never nil; nil (absent from the JSON) saves the rules without changing it. Saved, it
	// replaces the stored caps whole: a payee left out, or capped at zero, has no cap.
	PayeeDailyLimitsULXC map[string]int64 `json:"payee_daily_limits_ulxc"`
	// MaxCommitmentULXC, AllowedLicences and MaySubscribe (B32.22) bound the marketplace licences the agent may take:
	// the most one may commit it to (a buy's or a rent's price, one subscription period's), the licences it may hold —
	// commercial and enterprise; personal is never an agent's, and empty allows both — and whether it may subscribe at
	// all — or take a rent that renews itself — which it may not until its owner says so. Read, they are never nil; nil
	// (absent from the JSON) saves the rules without changing them.
	MaxCommitmentULXC *int64   `json:"max_commitment_ulxc"`
	AllowedLicences   []string `json:"allowed_licences"`
	MaySubscribe      *bool    `json:"may_subscribe"`
}

// AgentLicences are the marketplace licences an agent may hold (B32.22): a personal licence is one person's.
var AgentLicences = []string{"commercial", "enterprise"}

// Commitment is what a marketplace licence commits an agent to (B32.22): its kind — buy, rent or subscribe — its
// licence, its price — a buy's or a rent's, or one subscription period's — and whether it renews itself, as a
// subscription and a rent set to renew do. A use commits to nothing beyond itself, and is the zero Commitment.
type Commitment struct {
	Kind    string `json:"kind"`
	Licence string `json:"licence"`
	Renews  bool   `json:"renews"`
	ULXC    int64  `json:"-"`
}

// AgentRequest is what the rules judge about a request besides its amount.
type AgentRequest struct {
	Model       string
	Provider    string
	Fingerprint string    // the key, model and prompt, hashed: what an approval is for
	At          time.Time // zero means now
	Payment     bool      // a payment to another agent (B19.3) or a card purchase (B19.12): no model or provider to judge
	Listing     string    // the marketplace listing a use is of (B19.14)
	Payee       Payee     // who a payment goes to, and Memo why: the approval it files names both (B23.5)
	Memo        string
	Commitment  Commitment // the marketplace licence a payment takes, if it takes one (B32.22)
}

// Payee is who a payment goes to. Name is the one it had when the approval was filed; empty until then
// (namePayee reads it) unless the caller already knows it.
type Payee struct {
	Kind string `json:"kind"` // agent | listing | company | merchant
	ID   string `json:"id"`
	Name string `json:"name"`
}

// namePayee reads p's name when the caller did not give one; a payee that no longer exists keeps none.
func namePayee(ctx context.Context, q pgxDB, p Payee) (Payee, error) {
	query := map[string]string{
		"agent":   `SELECT name FROM agent_accounts WHERE id = $1`,
		"listing": `SELECT title FROM market_listings WHERE id = $1`,
		"company": `SELECT name FROM workspaces WHERE id = $1`,
	}[p.Kind]
	if p.Name != "" || p.ID == "" || query == "" {
		return p, nil
	}
	if err := q.QueryRow(ctx, query, p.ID).Scan(&p.Name); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("economy: payee name: %w", err)
	}
	return p, nil
}

type agentRequestKey struct{}

// WithAgentRequest carries req to the agent's hold or debit made with ctx.
func WithAgentRequest(ctx context.Context, req AgentRequest) context.Context {
	return context.WithValue(ctx, agentRequestKey{}, req)
}

func agentRequestFrom(ctx context.Context) AgentRequest {
	req, _ := ctx.Value(agentRequestKey{}).(AgentRequest)
	if req.At.IsZero() {
		req.At = time.Now()
	}
	return req
}

// ApprovalNeededError is the refusal of a request above the agent's approval amount. ApprovalID is the
// approval filed for it, which the workspace's owner approves or denies.
type ApprovalNeededError struct {
	ApprovalID  string
	AmountULXC  int64
	workspaceID string
	agentID     string
	req         AgentRequest
}

func (e *ApprovalNeededError) Error() string {
	return fmt.Sprintf("this request would cost up to %s LXC, above the agent's approval amount — approval %s must be approved by the workspace's owner before it is retried",
		lxcString(e.AmountULXC), e.ApprovalID)
}

func (e *ApprovalNeededError) Unwrap() error { return ErrApprovalRequired }

func lxcString(ulxc int64) string { return strconv.FormatFloat(float64(ulxc)/1e6, 'f', -1, 64) }

func ruleRefusal(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAgentRule, fmt.Sprintf(format, args...))
}

func clockMinutes(hhmm string) (int, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return 0, fmt.Errorf("%q is not a time of day (HH:MM)", hhmm)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (r AgentRules) location() (*time.Location, error) {
	if r.Timezone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(r.Timezone)
}

// limitOf is a limit that may be absent: absent is no limit.
func limitOf(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func (r AgentRules) validate() error {
	for _, v := range []int64{r.MaxPerRequestULXC, limitOf(r.HourlyLimitULXC), r.DailyLimitULXC, limitOf(r.WeeklyLimitULXC),
		r.MonthlyLimitULXC, r.ApprovalAboveULXC, limitOf(r.RequestsPerMinute), limitOf(r.MaxCommitmentULXC)} {
		if v < 0 {
			return errors.New("a limit cannot be negative (0 means no limit)")
		}
	}
	named := map[string]string{}
	for model, v := range r.ModelDailyLimitsULXC {
		key := modelCapKey(model)
		if key == "" {
			return errors.New("a model's daily limit needs the model's name")
		}
		if v < 0 {
			return fmt.Errorf("the daily limit for %q cannot be negative (0 means no limit)", model)
		}
		if other, ok := named[key]; ok {
			return fmt.Errorf("%q and %q are the same model; give it one daily limit", other, model)
		}
		named[key] = model
	}
	if limitOf(r.RequestsPerMinute) > math.MaxInt32 {
		return errors.New("requests_per_minute is too large")
	}
	for _, id := range slices.Concat(r.AllowedPayees, r.BlockedPayees) {
		if strings.TrimSpace(id) != id || id == "" {
			return fmt.Errorf("%q is not a payee's id", id)
		}
	}
	for _, id := range r.AllowedPayees {
		if slices.Contains(r.BlockedPayees, id) {
			return fmt.Errorf("%q is both allowed and blocked; give it one", id)
		}
	}
	for id, v := range r.PayeeDailyLimitsULXC {
		if strings.TrimSpace(id) != id || id == "" {
			return fmt.Errorf("%q is not a payee's id", id)
		}
		if v < 0 {
			return fmt.Errorf("the daily limit for %q cannot be negative (0 means no limit)", id)
		}
	}
	for _, licence := range r.AllowedLicences {
		if !slices.Contains(AgentLicences, licence) {
			return fmt.Errorf("an agent may hold commercial and enterprise licences, not %q: a personal licence is one person's", licence)
		}
	}
	if (r.ActiveFrom == "") != (r.ActiveUntil == "") {
		return errors.New("active_from and active_until go together")
	}
	if r.ActiveFrom != "" {
		from, err := clockMinutes(r.ActiveFrom)
		if err != nil {
			return err
		}
		until, err := clockMinutes(r.ActiveUntil)
		if err != nil {
			return err
		}
		if from == until {
			return errors.New("active_from and active_until are the same time; leave both empty for all day")
		}
	}
	if _, err := r.location(); err != nil {
		return fmt.Errorf("timezone %q is not an IANA time zone", r.Timezone)
	}
	return nil
}

func nullIfZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

const agentRulesColumns = `COALESCE(max_per_request_ulxc, 0), COALESCE(daily_limit_ulxc, 0), COALESCE(monthly_limit_ulxc, 0),
	COALESCE(approval_above_ulxc, 0), allowed_models, allowed_providers, COALESCE(active_from, ''), COALESCE(active_until, ''), timezone,
	pause_on_unusual_spend, allowed_listings, COALESCE(hourly_limit_ulxc, 0), COALESCE(weekly_limit_ulxc, 0), model_daily_limits_ulxc,
	COALESCE(requests_per_minute, 0), allowed_payees, blocked_payees, payee_daily_limits_ulxc,
	COALESCE(max_commitment_ulxc, 0), COALESCE(allowed_licences, '{}'), COALESCE(may_subscribe, false)`

func scanAgentRules(row pgx.Row) (AgentRules, error) {
	var r AgentRules
	var hourly, weekly, rpm, commitment int64
	var maySubscribe bool
	err := row.Scan(&r.MaxPerRequestULXC, &r.DailyLimitULXC, &r.MonthlyLimitULXC, &r.ApprovalAboveULXC,
		&r.AllowedModels, &r.AllowedProviders, &r.ActiveFrom, &r.ActiveUntil, &r.Timezone, &r.PauseOnUnusualSpend, &r.AllowedListings,
		&hourly, &weekly, &r.ModelDailyLimitsULXC, &rpm, &r.AllowedPayees, &r.BlockedPayees, &r.PayeeDailyLimitsULXC,
		&commitment, &r.AllowedLicences, &maySubscribe)
	r.HourlyLimitULXC, r.WeeklyLimitULXC, r.RequestsPerMinute = &hourly, &weekly, &rpm
	r.MaxCommitmentULXC, r.MaySubscribe = &commitment, &maySubscribe
	return r, err
}

// modelCapSuffix is what a dated snapshot or a -latest alias adds to its model's name.
var modelCapSuffix = regexp.MustCompile(`-([0-9]{8}|[0-9]{4}-[0-9]{2}-[0-9]{2}|latest)$`)

// modelCapKey is the name a per-model daily cap knows a model by (B28.301): lower-case, without a dated
// snapshot's or a -latest alias's suffix, so an agent cannot step around its cap on claude-opus-4-1 by asking
// for Claude-Opus-4-1 or claude-opus-4-1-20250805. Caps are saved, judged and counted under it.
func modelCapKey(model string) string {
	return modelCapSuffix.ReplaceAllString(strings.ToLower(strings.TrimSpace(model)), "")
}

// modelLimitsJSON is the JSON a model's daily caps are saved as, by modelCapKey, with the zeros (no cap) left
// out; nil when absent.
func modelLimitsJSON(limits map[string]int64) (*string, error) {
	return capsJSON(limits, modelCapKey)
}

// capsJSON is the JSON daily caps are saved as, each under key(its name), with the zeros (no cap) left out; nil
// when absent. A payee's caps (B28.304) are saved under its id as given.
func capsJSON(limits map[string]int64, key func(string) string) (*string, error) {
	if limits == nil {
		return nil, nil
	}
	kept := map[string]int64{}
	for name, v := range limits {
		if v > 0 {
			kept[key(name)] = v
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	out := string(b)
	return &out, nil
}

// SetAgentRules replaces an agent's rules — all but AllowedListings, HourlyLimitULXC, WeeklyLimitULXC,
// ModelDailyLimitsULXC, RequestsPerMinute, AllowedPayees, BlockedPayees, PayeeDailyLimitsULXC, MaxCommitmentULXC,
// AllowedLicences and MaySubscribe when they are nil (absent from the JSON): a client that predates them (B19.14,
// B28.300, B28.301, B28.302, B28.303, B28.304, B32.22) must not clear an agent's listings, caps, payees or licence
// rules by saving its other rules. An empty list or a zero clears them. A save
// that changes them is their next version (B28.307), recorded with who ctx names (WithRulesChange).
func (s *DualTokenStore) SetAgentRules(ctx context.Context, workspaceID, agentID string, r AgentRules) (AgentRules, error) {
	if err := r.validate(); err != nil {
		return r, fmt.Errorf("%w: %s", ErrAgentRule, err)
	}
	if r.AllowedModels == nil {
		r.AllowedModels = []string{}
	}
	if r.AllowedProviders == nil {
		r.AllowedProviders = []string{}
	}
	if r.Timezone == "" {
		r.Timezone = "UTC"
	}
	var from, until any
	if r.ActiveFrom != "" {
		from, until = r.ActiveFrom, r.ActiveUntil
	}
	models, err := modelLimitsJSON(r.ModelDailyLimitsULXC)
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	payeeCaps, err := capsJSON(r.PayeeDailyLimitsULXC, func(id string) string { return id })
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// B32.32: a room wallet's monthly limit is the room's budget, at most what the room owner's plan allows.
	if err := s.checkRoomBudget(ctx, tx, workspaceID, agentID, r.MonthlyLimitULXC); err != nil {
		return r, err
	}
	// $14 to $23 are NULL when absent, which keeps the stored caps, payees and licence rules; a zero or an empty list
	// clears one.
	var hourly, weekly, rpm, commitment int64
	var maySubscribe bool
	err = tx.QueryRow(ctx, `
		INSERT INTO agent_rules (agent_id, workspace_id, max_per_request_ulxc, daily_limit_ulxc, monthly_limit_ulxc,
		       approval_above_ulxc, allowed_models, allowed_providers, active_from, active_until, timezone, pause_on_unusual_spend,
		       allowed_listings, hourly_limit_ulxc, weekly_limit_ulxc, model_daily_limits_ulxc, requests_per_minute,
		       allowed_payees, blocked_payees, payee_daily_limits_ulxc, max_commitment_ulxc, allowed_licences, may_subscribe)
		SELECT id, workspace_id, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, COALESCE($13::text[], '{}'),
		       NULLIF($14::bigint, 0), NULLIF($15::bigint, 0), COALESCE($16::text::jsonb, '{}'), NULLIF($17::integer, 0),
		       COALESCE($18::text[], '{}'), COALESCE($19::text[], '{}'), COALESCE($20::text::jsonb, '{}'),
		       NULLIF($21::bigint, 0), COALESCE($22::text[], '{}'), COALESCE($23::boolean, false)
		  FROM agent_accounts WHERE id = $1 AND workspace_id = $2
		ON CONFLICT (agent_id) DO UPDATE SET max_per_request_ulxc = EXCLUDED.max_per_request_ulxc,
		       daily_limit_ulxc = EXCLUDED.daily_limit_ulxc, monthly_limit_ulxc = EXCLUDED.monthly_limit_ulxc,
		       approval_above_ulxc = EXCLUDED.approval_above_ulxc, allowed_models = EXCLUDED.allowed_models,
		       allowed_providers = EXCLUDED.allowed_providers, active_from = EXCLUDED.active_from,
		       active_until = EXCLUDED.active_until, timezone = EXCLUDED.timezone,
		       pause_on_unusual_spend = EXCLUDED.pause_on_unusual_spend, allowed_listings = COALESCE($13::text[], agent_rules.allowed_listings),
		       hourly_limit_ulxc = NULLIF(COALESCE($14::bigint, agent_rules.hourly_limit_ulxc), 0),
		       weekly_limit_ulxc = NULLIF(COALESCE($15::bigint, agent_rules.weekly_limit_ulxc), 0),
		       model_daily_limits_ulxc = COALESCE($16::text::jsonb, agent_rules.model_daily_limits_ulxc),
		       requests_per_minute = NULLIF(COALESCE($17::integer, agent_rules.requests_per_minute), 0),
		       allowed_payees = COALESCE($18::text[], agent_rules.allowed_payees),
		       blocked_payees = COALESCE($19::text[], agent_rules.blocked_payees),
		       payee_daily_limits_ulxc = COALESCE($20::text::jsonb, agent_rules.payee_daily_limits_ulxc),
		       max_commitment_ulxc = NULLIF(COALESCE($21::bigint, agent_rules.max_commitment_ulxc), 0),
		       allowed_licences = COALESCE($22::text[], agent_rules.allowed_licences),
		       may_subscribe = COALESCE($23::boolean, agent_rules.may_subscribe), updated_at = now()
		RETURNING COALESCE(hourly_limit_ulxc, 0), COALESCE(weekly_limit_ulxc, 0), model_daily_limits_ulxc, COALESCE(requests_per_minute, 0),
		       allowed_payees, blocked_payees, payee_daily_limits_ulxc, COALESCE(max_commitment_ulxc, 0), allowed_licences, may_subscribe`,
		agentID, workspaceID, nullIfZero(r.MaxPerRequestULXC), nullIfZero(r.DailyLimitULXC), nullIfZero(r.MonthlyLimitULXC),
		nullIfZero(r.ApprovalAboveULXC), r.AllowedModels, r.AllowedProviders, from, until, r.Timezone, r.PauseOnUnusualSpend,
		r.AllowedListings, r.HourlyLimitULXC, r.WeeklyLimitULXC, models, r.RequestsPerMinute, r.AllowedPayees, r.BlockedPayees, payeeCaps,
		r.MaxCommitmentULXC, r.AllowedLicences, r.MaySubscribe).
		Scan(&hourly, &weekly, &r.ModelDailyLimitsULXC, &rpm, &r.AllowedPayees, &r.BlockedPayees, &r.PayeeDailyLimitsULXC,
			&commitment, &r.AllowedLicences, &maySubscribe)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrAgentNotFound
	}
	if err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	if err := recordRulesVersion(ctx, tx, agentID, rulesChangeFrom(ctx)); err != nil {
		return r, err
	}
	if err := tx.Commit(ctx); err != nil {
		return r, fmt.Errorf("economy: set agent rules: %w", err)
	}
	r.HourlyLimitULXC, r.WeeklyLimitULXC, r.RequestsPerMinute = &hourly, &weekly, &rpm
	r.MaxCommitmentULXC, r.MaySubscribe = &commitment, &maySubscribe
	return r, nil
}

// GetAgentRules reads an agent's rules; an agent with none set has none.
func (s *DualTokenStore) GetAgentRules(ctx context.Context, workspaceID, agentID string) (AgentRules, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_accounts WHERE id = $1 AND workspace_id = $2)`,
		agentID, workspaceID).Scan(&exists); err != nil {
		return AgentRules{}, fmt.Errorf("economy: agent rules: %w", err)
	}
	if !exists {
		return AgentRules{}, ErrAgentNotFound
	}
	r, err := scanAgentRules(s.pool.QueryRow(ctx, `SELECT `+agentRulesColumns+` FROM agent_rules WHERE agent_id = $1`, agentID))
	if errors.Is(err, pgx.ErrNoRows) {
		var hourly, weekly, rpm, commitment int64
		var maySubscribe bool
		return AgentRules{HourlyLimitULXC: &hourly, WeeklyLimitULXC: &weekly, RequestsPerMinute: &rpm, ModelDailyLimitsULXC: map[string]int64{},
			AllowedModels: []string{}, AllowedProviders: []string{}, AllowedListings: []string{}, AllowedPayees: []string{},
			BlockedPayees: []string{}, PayeeDailyLimitsULXC: map[string]int64{}, Timezone: "UTC",
			MaxCommitmentULXC: &commitment, AllowedLicences: []string{}, MaySubscribe: &maySubscribe}, nil
	}
	if err != nil {
		return r, fmt.Errorf("economy: agent rules: %w", err)
	}
	return r, nil
}

// agentPeriodLimit is one of an agent's spending limits over a period: what it may spend since `since`.
type agentPeriodLimit struct {
	ulxc  int64
	since time.Time
	name  string
}

// periodLimits are r's hourly, daily, weekly and monthly limits, their periods begun in loc as of now (in
// loc): the clock hour, the day, the week from Monday and the month. The hour is counted back from now, not
// rebuilt with time.Date: when the clocks go back an hour repeats, and time.Date may answer its other
// occurrence — one in the future, which would count nothing spent.
func (r AgentRules) periodLimits(now time.Time, loc *time.Location) []agentPeriodLimit {
	monday := now.Day() - (int(now.Weekday())+6)%7
	hour := now.Add(-(time.Duration(now.Minute())*time.Minute + time.Duration(now.Second())*time.Second + time.Duration(now.Nanosecond())))
	return []agentPeriodLimit{
		{limitOf(r.HourlyLimitULXC), hour, "hourly"},
		{r.DailyLimitULXC, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc), "daily"},
		{limitOf(r.WeeklyLimitULXC), time.Date(now.Year(), now.Month(), monday, 0, 0, 0, 0, loc), "weekly"},
		{r.MonthlyLimitULXC, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc), "monthly"},
	}
}

// agentSpentSince is what an agent has spent since `since`, as its period limits count it.
func agentSpentSince(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, since time.Time) (int64, error) {
	var spent int64
	// What it paid other agents counts (B19.3), and its card purchases (B19.12); what they paid it does not raise its limit. What it
	// used of paid marketplace listings counts too (B20.2), though it is billed to its company. Its questions' platform fees count (B32.11).
	// So do its payments out of Talyvor (B30.17) at what they were worth in LXC, unless they failed or came back, and the
	// invoices it paid from its currency account (B30.131), recorded under their entry key as the rules let them through.
	err := tx.QueryRow(ctx, `SELECT (
		(SELECT COALESCE(-sum(amount_ulxc), 0) FROM agent_postings
		 WHERE workspace_id = $1 AND account = $2 AND created_at >= $3
		   AND (kind IN ('spend', 'hold', 'settle', 'release', 'card', 'platform_fee') OR (kind = 'pay' AND amount_ulxc < 0)))
		+ (SELECT COALESCE(sum(price_ulxc), 0) FROM market_uses
		   WHERE buyer_workspace_id = $1 AND agent_id = $4 AND charge = 'billed' AND used_at >= $3)
		+ (SELECT COALESCE(sum(value_ulxc), 0) FROM money_payments
		   WHERE workspace_id = $1 AND agent_id = $4 AND created_at >= $3 AND status NOT IN ('failed', 'returned'))
		+ (SELECT COALESCE(sum(amount_ulxc), 0) FROM agent_payee_payments
		   WHERE workspace_id = $1 AND agent_id = $4 AND ref LIKE 'invoice_agent:%' AND created_at >= $3))::bigint`,
		workspaceID, agentAccount(agentID), since, agentID).Scan(&spent)
	return spent, err
}

// modelDailyLimit is r's daily cap on model (B28.301), 0 for none, and when its day began in loc as of now.
func (r AgentRules) modelDailyLimit(model string, now time.Time, loc *time.Location) (int64, time.Time) {
	return r.ModelDailyLimitsULXC[modelCapKey(model)], time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
}

// agentModelSpentSince is what an agent has spent on model since `since`: its questions to it, held or debited,
// net of what their settles and releases gave back, with their platform fees (B32.11) — the postings that name
// the model (B28.301).
func agentModelSpentSince(ctx context.Context, tx pgx.Tx, workspaceID, agentID, model string, since time.Time) (int64, error) {
	var spent int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(-sum(amount_ulxc), 0)::bigint FROM agent_postings
		WHERE workspace_id = $1 AND account = $2 AND model = $3 AND created_at >= $4 AND kind IN ('spend', 'hold', 'settle', 'release', 'platform_fee')`,
		workspaceID, agentAccount(agentID), modelCapKey(model), since).Scan(&spent)
	return spent, err
}

// agentRequestsSince is how many questions an agent has held or debited since `since` (B28.302): one per
// reservation or claim, counted by its first posting — a debit's later under-charge settle is a 'spend' of the
// same ref, not another question. A settle is looked for no further back than an hour.
func agentRequestsSince(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, since time.Time) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT ref FROM agent_postings
		WHERE workspace_id = $1 AND account = $2 AND kind IN ('hold', 'spend') AND created_at > $3::timestamptz - interval '1 hour'
		GROUP BY ref HAVING min(created_at) > $3) AS questions`,
		workspaceID, agentAccount(agentID), since).Scan(&n)
	return n, err
}

// payeeIDs are the ids a payee lists may name p by (B28.303): its own, and for an agent or a listing the company
// that has it, so that naming a company names its agents and listings too.
func payeeIDs(ctx context.Context, tx pgx.Tx, p Payee) ([]string, error) {
	query := map[string]string{
		"agent":   `SELECT workspace_id FROM agent_accounts WHERE id = $1`,
		"listing": `SELECT workspace_id FROM market_listings WHERE id = $1`,
	}[p.Kind]
	if query == "" || p.ID == "" {
		return []string{p.ID}, nil
	}
	var company string
	if err := tx.QueryRow(ctx, query, p.ID).Scan(&company); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("economy: payee's company: %w", err)
	}
	return []string{p.ID, company}, nil
}

// agentPaidPayeeSince is what an agent has paid the payee a daily cap names by id since `since` (B28.304): its
// payments the rules let through, to that payee or — for a company — to its agents and listings.
func agentPaidPayeeSince(ctx context.Context, tx pgx.Tx, agentID, id string, since time.Time) (int64, error) {
	var paid int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(sum(amount_ulxc), 0)::bigint FROM agent_payee_payments
		WHERE agent_id = $1 AND $2 = ANY(payee_ids) AND created_at >= $3`, agentID, id, since).Scan(&paid)
	return paid, err
}

// recordPayeePayment records a payment the rules let through, in its movement's transaction, under the ids a
// payee's daily cap may name it by (B28.304) — every payment, capped or not, so a cap set at noon counts the
// morning. A payment of nothing, or to no one, is not one.
func recordPayeePayment(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, req AgentRequest, ids []string, amount int64, ref string) error {
	if !req.Payment || req.Payee.ID == "" || amount <= 0 {
		return nil
	}
	if ids == nil {
		var err error
		if ids, err = payeeIDs(ctx, tx, req.Payee); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_payee_payments (workspace_id, agent_id, payee_kind, payee_ids, amount_ulxc, ref)
		VALUES ($1, $2, $3, $4, $5, $6)`, workspaceID, agentID, req.Payee.Kind, slices.DeleteFunc(ids, func(id string) bool { return id == "" }),
		amount, ref); err != nil {
		return fmt.Errorf("economy: record payee payment: %w", err)
	}
	return nil
}

// CheckAgentRules judges an agent key's request that moves no LXC — one answered on its workspace's own
// provider key under BYOK (B27.26) — against the agent's rules: paused, active hours, models, providers. The
// amount rules pass at zero. A key attached to no agent has none. The request rides ctx (WithAgentRequest).
func (s *DualTokenStore) CheckAgentRules(ctx context.Context, scopedKeyID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var agentID, workspaceID string
	err = tx.QueryRow(ctx, `SELECT a.id, a.workspace_id FROM agent_account_keys k JOIN agent_accounts a ON a.id = k.agent_id
		WHERE k.scoped_key_id = $1`, scopedKeyID).Scan(&agentID, &workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("economy: agent of key: %w", err)
	}
	if err := enforceAgentRules(ctx, tx, workspaceID, agentID, 0, "byok"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// enforceAgentRules judges a positive movement of amount µLXC (a hold or a debit, ref its reservation or
// request id) against the agent's rules, inside the movement's transaction with the agent's row locked.
// A paused agent is refused first; a movement the rules let through is then watched for unusual spend (B19.6).
func enforceAgentRules(ctx context.Context, tx pgx.Tx, workspaceID, agentID string, amount int64, ref string) error {
	if err := refuseIfPaused(ctx, tx, agentID); err != nil {
		return err
	}
	req := agentRequestFrom(ctx)
	r, err := agentRulesInForce(ctx, tx, agentID, req.At) // with its boosts in force then (B28.308)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := (AgentRules{}).judgeCommitment(req.Commitment); err != nil {
			return err
		}
		if err := recordPayeePayment(ctx, tx, workspaceID, agentID, req, nil, amount, ref); err != nil {
			return err
		}
		return watchAgentSpend(ctx, tx, workspaceID, agentID, amount, req.At, false)
	}
	if err != nil {
		return fmt.Errorf("economy: agent rules: %w", err)
	}
	what := "request"
	if req.Payment {
		what = "payment"
	}
	loc, err := r.location()
	if err != nil {
		return ruleRefusal("the agent's timezone %q cannot be read", r.Timezone)
	}
	now := req.At.In(loc)
	if r.ActiveFrom != "" {
		from, _ := clockMinutes(r.ActiveFrom)
		until, _ := clockMinutes(r.ActiveUntil)
		m := now.Hour()*60 + now.Minute()
		inside := from <= m && m < until
		if from > until { // the window crosses midnight
			inside = m >= from || m < until
		}
		if !inside {
			return ruleRefusal("the agent may spend only between %s and %s (%s)", r.ActiveFrom, r.ActiveUntil, r.Timezone)
		}
	}
	if len(r.AllowedModels) > 0 && !req.Payment && !slices.Contains(r.AllowedModels, req.Model) {
		return ruleRefusal("the agent may not use the model %q", req.Model)
	}
	if len(r.AllowedProviders) > 0 && !req.Payment && !slices.Contains(r.AllowedProviders, req.Provider) {
		return ruleRefusal("the agent may not use the provider %q", req.Provider)
	}
	if len(r.AllowedListings) > 0 && req.Listing != "" && !slices.Contains(r.AllowedListings, req.Listing) {
		return ruleRefusal("the agent may not use the marketplace listing %q", req.Listing)
	}
	if err := r.judgeCommitment(req.Commitment); err != nil {
		return err
	}
	var ids []string // the ids the payee lists and caps name a payment's payee by
	if req.Payment && len(r.AllowedPayees)+len(r.BlockedPayees)+len(r.PayeeDailyLimitsULXC) > 0 {
		if ids, err = payeeIDs(ctx, tx, req.Payee); err != nil {
			return err
		}
	}
	if req.Payment && len(r.AllowedPayees)+len(r.BlockedPayees) > 0 {
		if slices.ContainsFunc(ids, func(id string) bool { return slices.Contains(r.BlockedPayees, id) }) {
			return ruleRefusal("the agent may not pay %s %q", req.Payee.Kind, req.Payee.ID)
		}
		if len(r.AllowedPayees) > 0 && !slices.ContainsFunc(ids, func(id string) bool { return slices.Contains(r.AllowedPayees, id) }) {
			return ruleRefusal("the agent may pay only the payees its rules name, and %s %q is not one", req.Payee.Kind, req.Payee.ID)
		}
	}
	if rpm := limitOf(r.RequestsPerMinute); rpm > 0 && !req.Payment {
		asked, err := agentRequestsSince(ctx, tx, workspaceID, agentID, req.At.Add(-time.Minute))
		if err != nil {
			return fmt.Errorf("economy: agent requests so far: %w", err)
		}
		if asked >= rpm {
			return fmt.Errorf("%w: the agent may make %d requests a minute and has made %d in the last minute; try again shortly",
				ErrAgentRequestRate, rpm, asked)
		}
	}
	if r.MaxPerRequestULXC > 0 && amount > r.MaxPerRequestULXC {
		return ruleRefusal("this %s would cost up to %s LXC; the agent's limit per request is %s LXC",
			what, lxcString(amount), lxcString(r.MaxPerRequestULXC))
	}
	for _, limit := range r.periodLimits(now, loc) {
		if limit.ulxc == 0 {
			continue
		}
		spent, err := agentSpentSince(ctx, tx, workspaceID, agentID, limit.since)
		if err != nil {
			return fmt.Errorf("economy: agent spend so far: %w", err)
		}
		if spent+amount > limit.ulxc {
			return ruleRefusal("the agent has spent %s LXC of its %s limit of %s LXC, and this %s would cost up to %s LXC",
				lxcString(spent), limit.name, lxcString(limit.ulxc), what, lxcString(amount))
		}
	}
	if limit, since := r.modelDailyLimit(req.Model, now, loc); limit > 0 && !req.Payment {
		spent, err := agentModelSpentSince(ctx, tx, workspaceID, agentID, req.Model, since)
		if err != nil {
			return fmt.Errorf("economy: agent spend on the model so far: %w", err)
		}
		if spent+amount > limit {
			return ruleRefusal("the agent has spent %s LXC of its daily limit of %s LXC for the model %q, and this request would cost up to %s LXC",
				lxcString(spent), lxcString(limit), req.Model, lxcString(amount))
		}
	}
	for _, id := range ids {
		limit := r.PayeeDailyLimitsULXC[id]
		if limit <= 0 {
			continue
		}
		paid, err := agentPaidPayeeSince(ctx, tx, agentID, id, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc))
		if err != nil {
			return fmt.Errorf("economy: agent payments to the payee so far: %w", err)
		}
		if paid+amount > limit {
			return ruleRefusal("the agent has paid %s LXC today of its daily limit of %s LXC for %q, and this payment would cost %s LXC",
				lxcString(paid), lxcString(limit), id, lxcString(amount))
		}
	}
	if r.ApprovalAboveULXC > 0 && amount > r.ApprovalAboveULXC {
		if req.Fingerprint == "" {
			return ruleRefusal("this %s needs approval and carries nothing to approve it by", what)
		}
		// An approved request goes through once: the approval is used up in this transaction, so a
		// refusal later in it (the workspace's balance, say) leaves it approved.
		tag, err := tx.Exec(ctx, `UPDATE agent_approvals SET status = 'used', used_ref = $3
			WHERE agent_id = $1 AND fingerprint = $2 AND status = 'approved'`, agentID, req.Fingerprint, ref)
		if err != nil {
			return fmt.Errorf("economy: use approval: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return &ApprovalNeededError{AmountULXC: amount, workspaceID: workspaceID, agentID: agentID, req: req}
		}
	}
	if err := recordPayeePayment(ctx, tx, workspaceID, agentID, req, ids, amount, ref); err != nil {
		return err
	}
	return watchAgentSpend(ctx, tx, workspaceID, agentID, amount, req.At, r.PauseOnUnusualSpend)
}

// judgeCommitment judges the marketplace licence a payment takes, if it takes one (B32.22), against r — the zero rules
// for an agent with none, which let it take a commercial or an enterprise licence at any price but never subscribe.
// Each refusal names its rule.
func (r AgentRules) judgeCommitment(c Commitment) error {
	switch {
	case c.Kind == "":
		return nil
	case !slices.Contains(AgentLicences, c.Licence):
		return ruleRefusal("an agent may hold only commercial and enterprise licences, and this %s is %s: a personal licence is one person's", c.Kind, c.Licence)
	case len(r.AllowedLicences) > 0 && !slices.Contains(r.AllowedLicences, c.Licence):
		return ruleRefusal("the agent may hold only %s licences (allowed_licences), and this %s is %s",
			strings.Join(r.AllowedLicences, " and "), c.Kind, c.Licence)
	case c.Kind == "subscribe" && (r.MaySubscribe == nil || !*r.MaySubscribe):
		return ruleRefusal("the agent's rules do not let it subscribe (may_subscribe)")
	case c.Renews && (r.MaySubscribe == nil || !*r.MaySubscribe): // a rent that renews itself is a subscription
		return ruleRefusal("the agent's rules do not let it take a %s that renews itself (may_subscribe)", c.Kind)
	case limitOf(r.MaxCommitmentULXC) > 0 && c.ULXC > *r.MaxCommitmentULXC:
		return ruleRefusal("this %s would commit the agent to %s LXC; the most one licence may commit it to is %s LXC (max_commitment_ulxc)",
			c.Kind, lxcString(c.ULXC), lxcString(*r.MaxCommitmentULXC))
	}
	return nil
}

// fileApproval records the approval a refused request needs — or finds the one already open for it — once
// the refused transaction has rolled back, and returns the refusal naming it.
func (s *DualTokenStore) fileApproval(ctx context.Context, need *ApprovalNeededError) error {
	payee, err := namePayee(ctx, s.pool, need.req.Payee)
	if err != nil {
		return err
	}
	var filed string
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO agent_approvals (id, workspace_id, agent_id, fingerprint, amount_ulxc, model, payee_kind, payee_id, payee_name, memo)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (agent_id, fingerprint) WHERE status IN ('pending', 'approved') DO NOTHING RETURNING id`,
		"apr_"+uuid.NewString(), need.workspaceID, need.agentID, need.req.Fingerprint, need.AmountULXC, need.req.Model,
		payee.Kind, payee.ID, payee.Name, need.req.Memo).Scan(&filed); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("economy: file approval: %w", err)
	}
	if filed != "" {
		s.notifyApproval(need.workspaceID, filed) // B19.16: a retry finds the open one and tells nobody again
	}
	if err := s.pool.QueryRow(ctx, `SELECT id FROM agent_approvals WHERE agent_id = $1 AND fingerprint = $2
		AND status IN ('pending', 'approved')`, need.agentID, need.req.Fingerprint).Scan(&need.ApprovalID); err != nil {
		return fmt.Errorf("economy: read approval: %w", err)
	}
	return need
}

// refusedMovement is what a hold or debit returns when agentMovement refused it: a request that needs
// approval files it (after rolling back, so nothing of the refused movement is kept); anything else as is.
func (s *DualTokenStore) refusedMovement(ctx context.Context, tx pgx.Tx, err error) error {
	var need *ApprovalNeededError
	if !errors.As(err, &need) {
		return err
	}
	_ = tx.Rollback(ctx)
	return s.fileApproval(ctx, need)
}

// AgentApproval is one request that needed a person's approval.
type AgentApproval struct {
	ID         string     `json:"id"`
	AgentID    string     `json:"agent_id"`
	AmountULXC int64      `json:"amount_ulxc"`
	Model      string     `json:"model"`
	Reason     string     `json:"reason,omitempty"` // why the agent asked (B19.9)
	Payee      *Payee     `json:"payee,omitempty"`  // who a payment goes to (B23.5); none for a request to a model
	Memo       string     `json:"memo,omitempty"`   // the payment's memo
	Status     string     `json:"status"`           // pending | approved | denied | used
	CreatedAt  time.Time  `json:"created_at"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
	// ApprovedByAgentID is the approver agent that approved it within its own limits (B45.28); empty for the owner.
	ApprovedByAgentID string `json:"approved_by_agent_id,omitempty"`
}

const agentApprovalColumns = `id, agent_id, amount_ulxc, model, reason, payee_kind, payee_id, payee_name, memo, status, created_at, decided_at,
	approved_by_agent_id`

func scanAgentApproval(row pgx.Row) (AgentApproval, error) {
	var a AgentApproval
	var p Payee
	err := row.Scan(&a.ID, &a.AgentID, &a.AmountULXC, &a.Model, &a.Reason, &p.Kind, &p.ID, &p.Name, &a.Memo, &a.Status, &a.CreatedAt, &a.DecidedAt,
		&a.ApprovedByAgentID)
	if p.Kind != "" {
		a.Payee = &p
	}
	return a, err
}

// ListAgentApprovals reads a workspace's approvals, newest first.
func (s *DualTokenStore) ListAgentApprovals(ctx context.Context, workspaceID string) ([]AgentApproval, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+agentApprovalColumns+` FROM agent_approvals WHERE workspace_id = $1
		ORDER BY created_at DESC, id LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: approvals: %w", err)
	}
	defer rows.Close()
	out := []AgentApproval{}
	for rows.Next() {
		a, err := scanAgentApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideAgentApproval approves or denies a pending approval.
func (s *DualTokenStore) DecideAgentApproval(ctx context.Context, workspaceID, approvalID string, approve bool) (AgentApproval, error) {
	status := "denied"
	if approve {
		status = "approved"
	}
	a, err := scanAgentApproval(s.pool.QueryRow(ctx, `UPDATE agent_approvals SET status = $3, decided_at = now()
		WHERE id = $1 AND workspace_id = $2 AND status = 'pending' RETURNING `+agentApprovalColumns, approvalID, workspaceID, status))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrApprovalNotFound
	}
	if err != nil {
		return a, fmt.Errorf("economy: decide approval: %w", err)
	}
	return a, nil
}
