package economy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// prompt_schedules.go — B28.125: A PROMPT SCHEDULED FROM CHAT, ASKED AT THE TIME SET ON AN AGENT'S OWN WALLET.
//
// RunPromptSchedules is called on a tick (cmd/lens, every minute). For each schedule due it first advances the
// schedule — or ends one that runs once — in a transaction holding its row, and only then asks the model, through
// the proxy as the agent's own key: so the agent's rules judge it and its wallet pays, exactly as a call the agent
// makes itself, and what it cost is a line on the agent's statement. A time asked is never asked twice, by a restart
// or a second Lens; a Lens that stops mid-question loses that run rather than paying for it twice.

// ErrPromptScheduleNotFound: no such prompt schedule in this workspace.
var ErrPromptScheduleNotFound = errors.New("economy: no such prompt schedule in this workspace")

// ErrBadPromptSchedule: a prompt schedule missing something it needs, saying what.
var ErrBadPromptSchedule = errors.New("economy: this prompt cannot be scheduled")

// maxPromptChars is the longest prompt a schedule keeps, as Chat's tasks.
const maxPromptChars = 8000

// promptSchedulesPerRun bounds one tick's questions; the rest wait for the next tick.
const promptSchedulesPerRun = 100

// promptScheduleSkew is how far in the past a first time may be and still be taken as now, as Chat's BFF allows.
const promptScheduleSkew = 2 * time.Minute

// promptAskTimeout bounds one question, so one slow model does not hold up the tick.
const promptAskTimeout = 2 * time.Minute

// PromptSchedule is a prompt asked of Model at NextRunAt, then every day or week while Active, paid by AgentID.
type PromptSchedule struct {
	ID        string      `json:"id"`
	AgentID   string      `json:"agent_id"`
	Prompt    string      `json:"prompt"`
	Provider  string      `json:"provider"`
	Model     string      `json:"model"`
	Every     string      `json:"every"` // once | day | week
	NextRunAt *time.Time  `json:"next_run_at,omitempty"`
	Active    bool        `json:"active"`
	CreatedAt time.Time   `json:"created_at"`
	Runs      []PromptRun `json:"runs"`
}

// PromptRun is one time a schedule was asked: answered, with what the agent's wallet paid in all and the statement
// line it paid on, or refused with Lens's sentence and no charge.
type PromptRun struct {
	RanAt       time.Time `json:"ran_at"`
	Outcome     string    `json:"outcome"` // answered | refused
	Answer      string    `json:"answer,omitempty"`
	Detail      string    `json:"detail,omitempty"`
	RequestID   string    `json:"request_id,omitempty"`
	ChargedULXC int64     `json:"charged_ulxc"`
	EntryID     string    `json:"entry_id,omitempty"`
}

// PromptAsker asks a model a prompt as one of a workspace's keys, through Lens's own proxy, under requestID.
// A refusal — the key's agent's rules, its balance, the provider — is an error carrying Lens's sentence.
// *proxy.Proxy satisfies it.
type PromptAsker interface {
	AskAsKey(ctx context.Context, workspaceID, scopedKeyID, provider, model, prompt, requestID string) (string, error)
}

var promptScheduleProviders = map[string]bool{"openai": true, "anthropic": true, "google": true, "bedrock": true, "mistral": true, "groq": true, "vllm": true}

// CreatePromptSchedule schedules prompt to be asked of provider/model on agentID's wallet at firstRunAt, then every
// day or week (every "once": that time only).
func (s *DualTokenStore) CreatePromptSchedule(ctx context.Context, workspaceID, agentID, prompt, provider, model, every string,
	firstRunAt time.Time) (PromptSchedule, error) {
	prompt = strings.TrimSpace(prompt)
	switch {
	case prompt == "":
		return PromptSchedule{}, fmt.Errorf("%w: a scheduled prompt needs saying what to ask", ErrBadPromptSchedule)
	case utf8.RuneCountInString(prompt) > maxPromptChars:
		return PromptSchedule{}, fmt.Errorf("%w: a scheduled prompt is at most 8,000 characters", ErrBadPromptSchedule)
	case !promptScheduleProviders[provider]:
		return PromptSchedule{}, fmt.Errorf("%w: unknown provider %q", ErrBadPromptSchedule, provider)
	case strings.TrimSpace(model) == "" || len(model) > 200:
		return PromptSchedule{}, fmt.Errorf("%w: a scheduled prompt needs a model", ErrBadPromptSchedule)
	case every != "once" && every != "day" && every != "week":
		return PromptSchedule{}, fmt.Errorf("%w: every must be once, day or week", ErrBadPromptSchedule)
	case firstRunAt.IsZero():
		return PromptSchedule{}, fmt.Errorf("%w: a scheduled prompt needs the time it runs", ErrBadPromptSchedule)
	case firstRunAt.Before(time.Now().Add(-promptScheduleSkew)):
		return PromptSchedule{}, fmt.Errorf("%w: that time has passed — pick a time to come", ErrBadPromptSchedule)
	}
	sc := PromptSchedule{Runs: []PromptRun{}}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO prompt_schedules (id, workspace_id, agent_id, prompt, provider, model, every, next_run_at)
		SELECT $1, $2, a.id, $4, $5, $6, $7, $8 FROM agent_accounts a WHERE a.id = $3 AND a.workspace_id = $2
		RETURNING id, agent_id, prompt, provider, model, every, next_run_at, active, created_at`,
		"psc_"+uuid.NewString(), workspaceID, agentID, prompt, provider, model, every, firstRunAt.UTC()).
		Scan(&sc.ID, &sc.AgentID, &sc.Prompt, &sc.Provider, &sc.Model, &sc.Every, &sc.NextRunAt, &sc.Active, &sc.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return sc, ErrAgentNotFound
	}
	if err != nil {
		return sc, fmt.Errorf("economy: create prompt schedule: %w", err)
	}
	return sc, nil
}

// ListPromptSchedules reads a workspace's prompt schedules, newest first, each with its runs, newest first.
func (s *DualTokenStore) ListPromptSchedules(ctx context.Context, workspaceID string) ([]PromptSchedule, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, agent_id, prompt, provider, model, every, next_run_at, active, created_at
		FROM prompt_schedules WHERE workspace_id = $1 ORDER BY created_at DESC, id LIMIT 200`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: prompt schedules: %w", err)
	}
	out := []PromptSchedule{}
	index := map[string]int{}
	for rows.Next() {
		sc := PromptSchedule{Runs: []PromptRun{}}
		if err := rows.Scan(&sc.ID, &sc.AgentID, &sc.Prompt, &sc.Provider, &sc.Model, &sc.Every, &sc.NextRunAt, &sc.Active, &sc.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		index[sc.ID] = len(out)
		out = append(out, sc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("economy: prompt schedules: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	ids := make([]string, len(out))
	for i, sc := range out {
		ids[i] = sc.ID
	}
	rows, err = s.pool.Query(ctx, `SELECT schedule_id, ran_at, outcome, answer, detail, request_id, charged_ulxc, COALESCE(entry_id::text, '')
		FROM prompt_schedule_runs WHERE schedule_id = ANY($1) ORDER BY ran_at DESC`, ids)
	if err != nil {
		return nil, fmt.Errorf("economy: prompt schedule runs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r PromptRun
		if err := rows.Scan(&id, &r.RanAt, &r.Outcome, &r.Answer, &r.Detail, &r.RequestID, &r.ChargedULXC, &r.EntryID); err != nil {
			return nil, err
		}
		out[index[id]].Runs = append(out[index[id]].Runs, r)
	}
	return out, rows.Err()
}

// StopPromptSchedule stops a prompt schedule; its runs are kept.
func (s *DualTokenStore) StopPromptSchedule(ctx context.Context, workspaceID, scheduleID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE prompt_schedules SET active = false, next_run_at = NULL WHERE id = $1 AND workspace_id = $2`,
		scheduleID, workspaceID)
	if err != nil {
		return fmt.Errorf("economy: stop prompt schedule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrPromptScheduleNotFound
	}
	return nil
}

// duePrompt is a schedule's time claimed by this tick, with the agent key it is asked as.
type duePrompt struct {
	id, workspaceID, keyID, prompt, provider, model string
}

// RunPromptSchedules asks every prompt schedule due at now, each time once, and returns how many were answered and
// refused. A failure to read or record one stops the run; the rest wait for the next tick.
func (s *DualTokenStore) RunPromptSchedules(ctx context.Context, now time.Time, ask PromptAsker) (answered, refused int, err error) {
	for i := 0; i < promptSchedulesPerRun; i++ {
		due, ok, err := s.claimDuePrompt(ctx, now)
		if err != nil || !ok {
			return answered, refused, err
		}
		run := PromptRun{RanAt: time.Now().UTC(), RequestID: "psr_" + uuid.NewString()}
		if due.keyID == "" {
			run.Outcome, run.Detail = "refused", "the agent has no key to ask with"
		} else {
			actx, cancel := context.WithTimeout(ctx, promptAskTimeout)
			answer, aerr := ask.AskAsKey(actx, due.workspaceID, due.keyID, due.provider, due.model, due.prompt, run.RequestID)
			cancel()
			if aerr != nil {
				run.Outcome, run.Detail = "refused", aerr.Error()
			} else {
				run.Outcome, run.Answer = "answered", answer
			}
		}
		if err := s.recordPromptRun(ctx, due, &run); err != nil {
			return answered, refused, err
		}
		if run.Outcome == "answered" {
			answered++
		} else {
			refused++
		}
	}
	return answered, refused, nil
}

// claimDuePrompt takes the earliest due schedule and moves it on — to its next day or week, or, run once, to its
// end — in one transaction holding its row, so no other tick asks this time again.
func (s *DualTokenStore) claimDuePrompt(ctx context.Context, now time.Time) (duePrompt, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return duePrompt{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var d duePrompt
	var agentID, every string
	var at time.Time
	err = tx.QueryRow(ctx, `SELECT id, workspace_id, agent_id, prompt, provider, model, every, next_run_at FROM prompt_schedules
		WHERE active AND next_run_at <= $1 ORDER BY next_run_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now).
		Scan(&d.id, &d.workspaceID, &agentID, &d.prompt, &d.provider, &d.model, &every, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("economy: due prompt schedule: %w", err)
	}
	// The next time after now: times missed while Lens was down are not asked late, each charged again.
	var next *time.Time
	if step := map[string]time.Duration{"day": 24 * time.Hour, "week": 7 * 24 * time.Hour}[every]; step > 0 {
		n := at.Add(step * (now.Sub(at)/step + 1))
		next = &n
	}
	if _, err := tx.Exec(ctx, `UPDATE prompt_schedules SET next_run_at = $2, active = $2::timestamptz IS NOT NULL WHERE id = $1`, d.id, next); err != nil {
		return d, false, fmt.Errorf("economy: advance prompt schedule: %w", err)
	}
	err = tx.QueryRow(ctx, agentKeySQL, agentID, d.workspaceID).Scan(&d.keyID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return d, false, fmt.Errorf("economy: prompt schedule's key: %w", err)
	}
	return d, true, tx.Commit(ctx)
}

// agentKeySQL reads an agent's oldest live key, as a room wallet's is read (rooms/wallet.go): the call is the agent's own.
const agentKeySQL = `SELECT a.scoped_key_id FROM agent_account_keys a JOIN workspace_api_keys k ON k.id::text = a.scoped_key_id
	WHERE a.agent_id = $1 AND k.workspace_id = $2 AND (k.expires_at IS NULL OR k.expires_at > now())
	ORDER BY a.created_at LIMIT 1`

// AgentKey is the key a scheduled prompt of agentID's is asked with, "" when the agent has no live key.
func (s *DualTokenStore) AgentKey(ctx context.Context, workspaceID, agentID string) (string, error) {
	var key string
	err := s.pool.QueryRow(ctx, agentKeySQL, agentID, workspaceID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("economy: agent's key: %w", err)
	}
	return key, nil
}

// recordPromptRun records run with what the request cost the agent: every posting on its account the request's hold
// or debit made — hold, settle, fee — summed, and the first of them the line it was charged on.
func (s *DualTokenStore) recordPromptRun(ctx context.Context, d duePrompt, run *PromptRun) error {
	var entry *string
	if run.Outcome == "answered" {
		if err := s.pool.QueryRow(ctx, `
			WITH refs AS (
			  SELECT reservation_id AS ref FROM lxc_reservations WHERE workspace_id = $1 AND request_id = $2
			  UNION SELECT debit_key FROM agent_debit_settlements WHERE workspace_id = $1 AND request_id = $2),
			lines AS (SELECT p.id, p.entry_id, p.amount_ulxc FROM agent_postings p JOIN agent_account_keys k ON p.account = 'agent:' || k.agent_id
			  WHERE p.workspace_id = $1 AND k.scoped_key_id = $3 AND p.ref IN (SELECT ref FROM refs))
			SELECT COALESCE(-sum(amount_ulxc), 0)::bigint, (SELECT entry_id::text FROM lines ORDER BY id LIMIT 1) FROM lines`,
			d.workspaceID, run.RequestID, d.keyID).Scan(&run.ChargedULXC, &entry); err != nil {
			return fmt.Errorf("economy: prompt run's charge: %w", err)
		}
		if entry != nil {
			run.EntryID = *entry
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO prompt_schedule_runs (schedule_id, ran_at, outcome, answer, detail, request_id, charged_ulxc, entry_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT DO NOTHING`,
		d.id, run.RanAt, run.Outcome, run.Answer, run.Detail, run.RequestID, run.ChargedULXC, entry); err != nil {
		return fmt.Errorf("economy: record prompt run: %w", err)
	}
	return nil
}
