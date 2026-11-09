package economy

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/net/publicsuffix"

	"github.com/talyvor/lens/internal/devicekey"
	"github.com/talyvor/lens/internal/passkey"
)

// negotiation_mandates.go — B52.10: COUNTER'S NEGOTIATION MANDATE, SIGNED BY THE OWNER, AND ITS TERM SHEETS.
//
// A negotiation is a task plus a mandate: what is bought or renewed, which side the owner is on, the RFQ, auction or
// quote it happens in, the target and the walk-away, must-haves, the trade-offs the owner accepts, a deadline, the
// counterparties and channels allowed, and the authority tier. It is signed the way a permission mandate is (B40.2):
// PrepareNegotiationMandate answers the canonical JSON and its challenge, and CreateNegotiationMandate keeps it only
// with an assertion over that challenge by one of the workspace's passkeys or device keys. Every use checks the
// signature again, so a mandate altered after it was signed allows nothing.
//
// The walk-away is the owner's alone: RecordNegotiationTerms refuses an outbound offer or message that names it, in
// any way a number is written, and an offer that reaches it. One side per deal (B54.7): the reference the owner types
// holds nothing until the counterparty's own signed agent, or an email its domain signed (B54.8), confirms it
// (ConfirmNegotiationDeal), so no one can squat an RFQ by typing it first; a confirmed deal holds at most one live mandate across every workspace, so Counter never
// acts for both sides of one deal or for two bidders in one RFQ, and a refusal in it names nobody. A deal is its
// reference and its two parties, told apart by the domains each workspace proved it trades as (ClaimNegotiationDomain),
// so a workspace confirming its own deal holds nothing and strangers sharing a reference block no one. Nothing here moves
// money. Class: rules_approvals_statements_pots, GREEN.

var (
	// ErrNegotiationMandate: terms a negotiation mandate cannot have, a signature that does not sign them, or an
	// offer outside them.
	ErrNegotiationMandate = errors.New("economy: invalid negotiation mandate")
	// ErrNegotiationNotFound: no such negotiation in this workspace.
	ErrNegotiationNotFound = errors.New("economy: no such negotiation")
	// ErrNegotiationOneSide: the counterparty already confirmed this deal to another party. Only the counterparty
	// is told; a workspace refused in a confirmed deal gets ErrNegotiationMandate, like any other refusal.
	ErrNegotiationOneSide = errors.New("economy: Counter acts for one party per deal")
)

// NegotiationMandateVersion names the canonical form signed.
const NegotiationMandateVersion = "talyvor.negotiation_mandate.v1"

// The kinds of task, the sides, channels and authority tiers.
const (
	NegotiationRenewal  = "renewal"
	NegotiationPurchase = "purchase"

	NegotiationBuy  = "buy"
	NegotiationSell = "sell"

	ChannelEmail   = "email"
	ChannelWebForm = "web_form"
	ChannelAgent   = "agent"

	AuthorityResearch  = "research_only"
	AuthorityNegotiate = "negotiate"
	AuthorityAccept    = "accept_inside_mandate"
)

// NegotiationTask is what is being bought or renewed.
type NegotiationTask struct {
	Kind string `json:"kind"` // renewal or purchase
	What string `json:"what"`
}

// NegotiationMandateTerms is what the owner signs, in this field order. Amounts are the currency's minor units.
type NegotiationMandateTerms struct {
	Version        string          `json:"version"`
	ID             string          `json:"id"`
	WorkspaceID    string          `json:"workspace_id"`
	Task           NegotiationTask `json:"task"`
	Side           string          `json:"side"` // buy or sell
	Deal           string          `json:"deal"` // the RFQ, auction or quote reference it happens in; "" for none
	Currency       string          `json:"currency"`
	TargetMinor    int64           `json:"target_minor"`
	WalkAwayMinor  int64           `json:"walk_away_minor"` // private: never shown to a counterparty, never in a message
	MustHaves      []string        `json:"must_haves"`
	TradeOffs      []string        `json:"trade_offs"`
	Deadline       time.Time       `json:"deadline"`
	Counterparties []string        `json:"counterparties"`
	Channels       []string        `json:"channels"`
	WebForms       []string        `json:"web_forms"` // the sites whose forms Counter may fill, when web_form is allowed
	Authority      string          `json:"authority"`
}

func (t *NegotiationMandateTerms) normalise() {
	t.Task.What = strings.TrimSpace(t.Task.What)
	t.Deal = strings.ToLower(strings.TrimSpace(t.Deal))
	t.Currency = strings.ToUpper(t.Currency)
	t.MustHaves, t.TradeOffs = sortedSet(t.MustHaves), sortedSet(t.TradeOffs)
	lower := func(in []string) []string {
		out := []string{}
		for _, s := range in {
			out = append(out, strings.ToLower(s))
		}
		return sortedSet(out)
	}
	t.Counterparties, t.Channels, t.WebForms = lower(t.Counterparties), lower(t.Channels), lower(t.WebForms)
	t.Deadline = t.Deadline.UTC().Truncate(time.Second)
}

func (t NegotiationMandateTerms) validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrNegotiationMandate, fmt.Sprintf(format, args...))
	}
	if t.Version != NegotiationMandateVersion {
		return bad("version must be %q", NegotiationMandateVersion)
	}
	if t.Task.Kind != NegotiationRenewal && t.Task.Kind != NegotiationPurchase {
		return bad(`task.kind must be "renewal" or "purchase"`)
	}
	if t.Task.What == "" {
		return bad("task.what names nothing: say what is being bought or renewed")
	}
	if _, ok := MoneyCurrencies[t.Currency]; !ok {
		return bad("%q is not a currency Talyvor holds", t.Currency)
	}
	if t.TargetMinor <= 0 || t.WalkAwayMinor <= 0 {
		return bad("target_minor and walk_away_minor must be more than 0")
	}
	switch t.Side {
	case NegotiationBuy:
		if t.TargetMinor > t.WalkAwayMinor {
			return bad("a buyer's target cannot be above their walk-away")
		}
	case NegotiationSell:
		if t.TargetMinor < t.WalkAwayMinor {
			return bad("a seller's target cannot be below their walk-away")
		}
	default:
		return bad(`side must be "buy" or "sell"`)
	}
	if t.Deadline.IsZero() {
		return bad("a negotiation needs a deadline")
	}
	if len(t.Counterparties) == 0 {
		return bad("name at least one counterparty Counter may contact")
	}
	if len(t.Channels) == 0 {
		return bad(`name at least one channel: "email", "web_form" or "agent"`)
	}
	for _, c := range t.Channels {
		if c != ChannelEmail && c != ChannelWebForm && c != ChannelAgent {
			return bad("%q is not a channel: use \"email\", \"web_form\" or \"agent\"", c)
		}
	}
	if slices.Contains(t.Channels, ChannelWebForm) != (len(t.WebForms) > 0) {
		return bad("web_forms lists the sites whose forms Counter may fill, and only when web_form is allowed")
	}
	if t.Authority != AuthorityResearch && t.Authority != AuthorityNegotiate && t.Authority != AuthorityAccept {
		return bad(`authority must be "research_only", "negotiate" or "accept_inside_mandate"`)
	}
	return nil
}

func (t NegotiationMandateTerms) canonical() string {
	b, _ := json.Marshal(t)
	return string(b)
}

func parseNegotiationCanonical(canonical string) (NegotiationMandateTerms, error) {
	var t NegotiationMandateTerms
	dec := json.NewDecoder(strings.NewReader(canonical))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return t, fmt.Errorf("%w: not JSON of the mandate's form: %v", ErrNegotiationMandate, err)
	}
	n := t
	n.normalise()
	if n.canonical() != canonical {
		return n, fmt.Errorf("%w: not in its canonical form, as prepare returned it", ErrNegotiationMandate)
	}
	return n, n.validate()
}

// NegotiationTerms is one offer or counter-offer, as structured terms.
type NegotiationTerms struct {
	PriceMinor   int64    `json:"price_minor"`
	TermMonths   int      `json:"term_months,omitempty"`
	Renewal      string   `json:"renewal,omitempty"`
	PaymentTerms string   `json:"payment_terms,omitempty"`
	SLA          string   `json:"sla,omitempty"`
	Extras       []string `json:"extras,omitempty"`
}

// NegotiationOffer is one line of a counterparty's term sheet: ours, with the message sent, or theirs, as received.
type NegotiationOffer struct {
	ID           int64            `json:"id"`
	Counterparty string           `json:"counterparty"`
	Party        string           `json:"party"` // ours or theirs
	Channel      string           `json:"channel"`
	Terms        NegotiationTerms `json:"terms"`
	Message      string           `json:"message,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
}

// NegotiationMandate is a signed mandate as kept, with its term sheet per counterparty. Only its owner reads it.
type NegotiationMandate struct {
	NegotiationMandateTerms
	Status string `json:"status"` // active, past_deadline or revoked
	// Where the deal was confirmed from: the counterparty agent's signing domain and its channel. Empty: unconfirmed,
	// so the deal reference holds nothing.
	DealConfirmedBy  string                        `json:"deal_confirmed_by,omitempty"`
	DealConfirmedVia string                        `json:"deal_confirmed_via,omitempty"`
	DealConfirmedAt  *time.Time                    `json:"deal_confirmed_at,omitempty"`
	Canonical        string                        `json:"canonical"`
	CredentialID     string                        `json:"credential_id,omitempty"`
	DeviceID         string                        `json:"device_id,omitempty"`
	CreatedBy        string                        `json:"created_by"`
	CreatedAt        time.Time                     `json:"created_at"`
	EndedAt          *time.Time                    `json:"ended_at,omitempty"`
	TermSheets       map[string][]NegotiationOffer `json:"term_sheets"`
	Messages         []NegotiationMessage          `json:"messages"` // the deal's timeline: what Counter sent and the replies
}

// PrepareNegotiationMandate checks t for workspaceID, gives it a new id, and answers the canonical JSON to sign, its
// challenge and the workspace's passkeys that may sign it. Nothing is kept until it is signed.
func (s *DualTokenStore) PrepareNegotiationMandate(ctx context.Context, workspaceID string, t NegotiationMandateTerms) (canonical, challenge string, credentialIDs []string, err error) {
	t.Version, t.ID, t.WorkspaceID = NegotiationMandateVersion, uuid.NewString(), workspaceID
	t.normalise()
	if err := t.validate(); err != nil {
		return "", "", nil, err
	}
	if !t.Deadline.After(time.Now()) {
		return "", "", nil, fmt.Errorf("%w: the deadline has passed", ErrNegotiationMandate)
	}
	keys, err := s.ListPasskeys(ctx, workspaceID)
	if err != nil {
		return "", "", nil, err
	}
	var devices int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_device_keys WHERE workspace_id = $1 AND revoked_at IS NULL`,
		workspaceID).Scan(&devices); err != nil {
		return "", "", nil, fmt.Errorf("economy: device keys: %w", err)
	}
	if len(keys) == 0 && devices == 0 {
		return "", "", nil, fmt.Errorf("%w: a mandate is signed with a passkey or the phone app's device key, and this workspace has neither: register one first", ErrNegotiationMandate)
	}
	credentialIDs = []string{}
	for _, k := range keys {
		credentialIDs = append(credentialIDs, k.CredentialID)
	}
	c := t.canonical()
	return c, MandateChallenge(c), credentialIDs, nil
}

// CreateNegotiationMandate keeps a mandate the owner signed: canonical as prepare answered it, and an assertion over
// its challenge by one of the workspace's passkeys or device keys. An unsigned or altered mandate, another
// workspace's, one already kept, or one in a deal a counterparty confirmed to another workspace is refused; the deal is
// looked up before the signature is checked, so a refusal in a held deal takes what any signature refusal takes.
func (s *DualTokenStore) CreateNegotiationMandate(ctx context.Context, workspaceID, by, canonical string, a *passkey.Assertion) (NegotiationMandate, error) {
	t, err := parseNegotiationCanonical(canonical)
	if err != nil {
		return NegotiationMandate{}, err
	}
	if t.WorkspaceID != workspaceID {
		return NegotiationMandate{}, fmt.Errorf("%w: the mandate is another workspace's", ErrNegotiationMandate)
	}
	if !t.Deadline.After(time.Now()) {
		return NegotiationMandate{}, fmt.Errorf("%w: the deadline has passed", ErrNegotiationMandate)
	}
	if a == nil {
		return NegotiationMandate{}, fmt.Errorf("%w: the mandate is not signed: sign its challenge with a passkey", ErrNegotiationMandate)
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockDeal(ctx, tx, t.Deal); err != nil {
			return err
		}
		held, err := dealHeld(ctx, tx, workspaceID, t, t.Counterparties)
		if err != nil {
			return err
		}
		if a.DeviceID != "" {
			if err := signDevice(ctx, tx, workspaceID, MandateChallenge(canonical), *a); err != nil {
				return err
			}
			a = &passkey.Assertion{DeviceID: a.DeviceID, Signature: a.Signature}
		} else {
			var spki []byte
			var count int64
			err := tx.QueryRow(ctx, `SELECT public_key, sign_count FROM workspace_passkeys WHERE credential_id = $1 AND workspace_id = $2 AND removed_at IS NULL FOR UPDATE`,
				a.CredentialID, workspaceID).Scan(&spki, &count)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: not a passkey of this workspace, or it was removed", passkey.ErrInvalid)
			}
			if err != nil {
				return fmt.Errorf("economy: passkey: %w", err)
			}
			next, err := s.verifyMandateSignature(canonical, *a, spki, uint32(count))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE workspace_passkeys SET sign_count = $2, last_used_at = now() WHERE credential_id = $1`,
				a.CredentialID, int64(next)); err != nil {
				return fmt.Errorf("economy: passkey: %w", err)
			}
		}
		if held {
			return errHeldElsewhere
		}
		_, err = tx.Exec(ctx, `INSERT INTO negotiation_mandates (id, workspace_id, deal_ref, deadline, canonical, credential_id, device_id,
			client_data_json, authenticator_data, signature, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			t.ID, workspaceID, t.Deal, t.Deadline, canonical, a.CredentialID, a.DeviceID, a.ClientDataJSON, a.AuthenticatorData, a.Signature, by)
		if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "23505" {
			return fmt.Errorf("%w: this mandate was already signed once; prepare a new one", ErrNegotiationMandate)
		}
		if err != nil {
			return fmt.Errorf("economy: negotiation mandate: %w", err)
		}
		return nil
	})
	if err != nil {
		return NegotiationMandate{}, err
	}
	return s.NegotiationMandate(ctx, workspaceID, t.ID)
}

// errHeldElsewhere names nobody, and is an invalid mandate like any other: who else Counter acts for is theirs.
var errHeldElsewhere = fmt.Errorf("%w: Counter can't take this negotiation", ErrNegotiationMandate)

// lockDeal serialises everything that holds or acts in deal, so its two sides cannot both get in at once.
func lockDeal(ctx context.Context, tx pgx.Tx, deal string) error {
	if deal == "" {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('negotiation_deal:' || $1))`, deal); err != nil {
		return fmt.Errorf("economy: negotiation mandate: %w", err)
	}
	return nil
}

// negotiationDomains answers the registrable domains workspaceID has proved it trades as.
func negotiationDomains(ctx context.Context, tx pgx.Tx, workspaceID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT domain FROM negotiation_domains WHERE workspace_id = $1 AND verified_at IS NOT NULL`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: negotiation domains: %w", err)
	}
	domains, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("economy: negotiation domains: %w", err)
	}
	return domains, nil
}

// dealHeld reports whether t's deal is confirmed to a live mandate that workspaceID's t may not stand beside. A deal is
// its reference and its two parties, never the reference alone: on t's side, another workspace's mandate confirmed by
// one of domains (two bidders to one counterparty); on the other side, a mandate of workspaceID itself, or one whose
// workspace proved a domain t names, or one that workspaceID's own proved domain confirmed (Counter acting for both
// parties). Only proved domains and confirmations count, never another mandate's own list of counterparties, so no one
// can name a stranger to block it. Domains are counted as their registrable domains, as a confirmation is. An empty
// deal is held by no one.
func dealHeld(ctx context.Context, tx pgx.Tx, workspaceID string, t NegotiationMandateTerms, domains []string) (bool, error) {
	if t.Deal == "" {
		return false, nil
	}
	own, err := negotiationDomains(ctx, tx, workspaceID)
	if err != nil {
		return false, err
	}
	reg := func(in []string) []string {
		out := make([]string, len(in))
		for i, d := range in {
			out[i] = RegistrableDomain(d)
		}
		return out
	}
	by, names := reg(domains), reg(t.Counterparties)
	rows, err := tx.Query(ctx, `SELECT m.workspace_id, m.canonical::jsonb->>'side', m.deal_confirmed_by,
		ARRAY(SELECT d.domain FROM negotiation_domains d WHERE d.workspace_id = m.workspace_id AND d.verified_at IS NOT NULL)
		FROM negotiation_mandates m
		WHERE m.deal_ref = $1 AND m.deal_confirmed_at IS NOT NULL AND m.ended_at IS NULL AND m.deadline > now()`, t.Deal)
	if err != nil {
		return false, fmt.Errorf("economy: negotiation mandate: %w", err)
	}
	defer rows.Close()
	overlaps := func(a, b []string) bool {
		return slices.ContainsFunc(a, func(x string) bool { return slices.Contains(b, x) })
	}
	for rows.Next() {
		var ws, side, confirmedBy string
		var theirs []string
		if err := rows.Scan(&ws, &side, &confirmedBy, &theirs); err != nil {
			return false, fmt.Errorf("economy: negotiation mandate: %w", err)
		}
		if side == t.Side {
			if ws != workspaceID && slices.Contains(by, confirmedBy) {
				return true, nil
			}
		} else if ws == workspaceID || overlaps(theirs, names) || slices.Contains(own, confirmedBy) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// RegistrableDomain is the domain a business registered, so agent.acme.example and procurement.acme.example are one
// counterparty, acme.example. A public suffix or an address is itself.
func RegistrableDomain(d string) string {
	d = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
	if net.ParseIP(strings.Trim(d, "[]")) != nil {
		return d
	}
	if r, err := publicsuffix.EffectiveTLDPlusOne(d); err == nil {
		return r
	}
	return d
}

// ConfirmNegotiationDeal records that the counterparty, proved to be domain over channel via (its agent, signed with
// key keyID, or an email its domain's DKIM signature holds for, B54.8, which passes no keyID), confirmed deal as
// mandate id's: the deal is then the mandate's alone, recorded as domain's registrable domain and via. For the agent
// channel the caller has checked that the agent speaks for that registrable domain (B54.16). The mandate must be live
// and allow via, domain must be one of its counterparties or under one, and deal must be its reference; otherwise it
// is not found, so a stranger learns nothing. The mandate's workspace must have proved a domain it trades as, and
// neither domain nor keyID may be its own (a domain it proved, or an agent that proved one): a workspace never
// confirms its own deal. A mandate is confirmed once; a deal already confirmed to a mandate it may not stand beside
// (dealHeld) is ErrNegotiationOneSide.
func (s *DualTokenStore) ConfirmNegotiationDeal(ctx context.Context, id, domain, keyID, deal, via string) error {
	var workspaceID string
	err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM negotiation_mandates WHERE id = $1`, id).Scan(&workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNegotiationNotFound
	}
	if err != nil {
		return fmt.Errorf("economy: negotiation mandate: %w", err)
	}
	t, err := s.signedNegotiation(ctx, workspaceID, id)
	domain = strings.ToLower(domain)
	if err != nil || !slices.Contains(t.Channels, via) || !slices.ContainsFunc(t.Counterparties, func(c string) bool {
		return domain == c || strings.HasSuffix(domain, "."+c)
	}) {
		return ErrNegotiationNotFound
	}
	if t.Deal == "" || strings.ToLower(strings.TrimSpace(deal)) != t.Deal {
		return fmt.Errorf("%w: that is not the deal reference this negotiation is in", ErrNegotiationMandate)
	}
	by := RegistrableDomain(domain)
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := lockDeal(ctx, tx, t.Deal); err != nil {
			return err
		}
		// A mandate past its deadline gives its deal up.
		if _, err := tx.Exec(ctx, `UPDATE negotiation_mandates SET ended_at = now(), ended_by = 'deadline'
			WHERE deal_ref = $1 AND ended_at IS NULL AND deadline <= now()`, t.Deal); err != nil {
			return fmt.Errorf("economy: negotiation mandate: %w", err)
		}
		own, err := negotiationDomains(ctx, tx, workspaceID)
		if err != nil {
			return err
		}
		var ownAgent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM negotiation_signers WHERE workspace_id = $1 AND (domain = $2 OR key_id = $3))`,
			workspaceID, domain, keyID).Scan(&ownAgent); err != nil {
			return fmt.Errorf("economy: negotiation mandate: %w", err)
		}
		if len(own) == 0 || slices.Contains(own, by) || ownAgent {
			return fmt.Errorf("%w: a deal is confirmed from the counterparty's side, to an owner who has proved the domain it trades as", ErrNegotiationMandate)
		}
		held, err := dealHeld(ctx, tx, workspaceID, t, []string{by})
		if err != nil {
			return err
		}
		if held {
			return fmt.Errorf("%w: this deal is already confirmed to another party", ErrNegotiationOneSide)
		}
		tag, err := tx.Exec(ctx, `UPDATE negotiation_mandates SET deal_confirmed_by = $2, deal_confirmed_via = $3, deal_confirmed_at = now()
			WHERE id = $1 AND ended_at IS NULL AND deadline > now() AND deal_confirmed_at IS NULL`, id, by, via)
		if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "23505" {
			return fmt.Errorf("%w: this deal is already confirmed to another party", ErrNegotiationOneSide)
		}
		if err != nil {
			return fmt.Errorf("economy: negotiation mandate: %w", err)
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		// Not confirmed now: ended, past its deadline, or confirmed before — by this counterparty, which is no change.
		var prev string
		err = tx.QueryRow(ctx, `SELECT deal_confirmed_by FROM negotiation_mandates WHERE id = $1 AND ended_at IS NULL AND deadline > now()`, id).Scan(&prev)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNegotiationNotFound
		case err != nil:
			return fmt.Errorf("economy: negotiation mandate: %w", err)
		case prev != by:
			return fmt.Errorf("%w: this negotiation's deal is already confirmed", ErrNegotiationOneSide)
		}
		return nil
	})
}

// NegotiationDomain is a domain a workspace trades as in Counter's negotiations.
type NegotiationDomain struct {
	Domain     string     `json:"domain"`
	Verified   bool       `json:"verified"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

// ClaimNegotiationDomain records that workspaceID trades as domain (its registrable domain). The claim counts for
// nothing until VerifyNegotiationDomain: that domain's own signed agent names the workspace.
func (s *DualTokenStore) ClaimNegotiationDomain(ctx context.Context, workspaceID, domain, by string) (NegotiationDomain, error) {
	d := RegistrableDomain(domain)
	if _, err := publicsuffix.EffectiveTLDPlusOne(d); err != nil || net.ParseIP(strings.Trim(d, "[]")) != nil {
		return NegotiationDomain{}, fmt.Errorf("%w: %q is not a domain a business registers", ErrNegotiationMandate, domain)
	}
	out := NegotiationDomain{Domain: d}
	err := s.pool.QueryRow(ctx, `INSERT INTO negotiation_domains (workspace_id, domain, claimed_by) VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, domain) DO UPDATE SET domain = EXCLUDED.domain RETURNING verified_at`, workspaceID, d, by).Scan(&out.VerifiedAt)
	if err != nil {
		return NegotiationDomain{}, fmt.Errorf("economy: negotiation domains: %w", err)
	}
	out.Verified = out.VerifiedAt != nil
	return out, nil
}

// VerifyNegotiationDomain records that the agent signing as domain with key keyID says workspaceID trades as it: a
// claim of workspaceID's for domain's registrable domain is then verified, and the agent is recorded as workspaceID's
// own. The caller has checked that the agent speaks for that registrable domain (B54.16). Without that claim it is not
// found.
func (s *DualTokenStore) VerifyNegotiationDomain(ctx context.Context, workspaceID, domain, keyID string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE negotiation_domains SET verified_at = COALESCE(verified_at, now()) WHERE workspace_id = $1 AND domain = $2`,
			workspaceID, RegistrableDomain(domain))
		if err != nil {
			return fmt.Errorf("economy: negotiation domains: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNegotiationNotFound
		}
		if _, err := tx.Exec(ctx, `INSERT INTO negotiation_signers (workspace_id, domain, key_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			workspaceID, domain, keyID); err != nil {
			return fmt.Errorf("economy: negotiation domains: %w", err)
		}
		return nil
	})
}

// NegotiationMandate reads one of workspaceID's mandates, with its term sheets.
func (s *DualTokenStore) NegotiationMandate(ctx context.Context, workspaceID, id string) (NegotiationMandate, error) {
	var m NegotiationMandate
	err := s.pool.QueryRow(ctx, `SELECT canonical, credential_id, device_id, created_by, created_at, ended_at, deal_confirmed_by,
		deal_confirmed_via, deal_confirmed_at FROM negotiation_mandates WHERE id = $1 AND workspace_id = $2`, id, workspaceID).
		Scan(&m.Canonical, &m.CredentialID, &m.DeviceID, &m.CreatedBy, &m.CreatedAt, &m.EndedAt, &m.DealConfirmedBy, &m.DealConfirmedVia, &m.DealConfirmedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNegotiationNotFound
	}
	if err != nil {
		return m, fmt.Errorf("economy: negotiation mandate: %w", err)
	}
	// The terms shown are the signed JSON's; a row whose JSON does not read is shown by id alone.
	if err := json.Unmarshal([]byte(m.Canonical), &m.NegotiationMandateTerms); err != nil {
		m.ID = id
	}
	m.Status = "active"
	switch {
	case m.EndedAt != nil:
		m.Status = "revoked"
	case !time.Now().Before(m.Deadline):
		m.Status = "past_deadline"
	}
	m.TermSheets = map[string][]NegotiationOffer{}
	rows, err := s.pool.Query(ctx, `SELECT id, counterparty, party, channel, terms, message, created_at FROM negotiation_terms
		WHERE mandate_id = $1 AND workspace_id = $2 ORDER BY id`, id, workspaceID)
	if err != nil {
		return m, fmt.Errorf("economy: negotiation terms: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var o NegotiationOffer
		if err := rows.Scan(&o.ID, &o.Counterparty, &o.Party, &o.Channel, &o.Terms, &o.Message, &o.CreatedAt); err != nil {
			return m, fmt.Errorf("economy: negotiation terms: %w", err)
		}
		m.TermSheets[o.Counterparty] = append(m.TermSheets[o.Counterparty], o)
	}
	if err := rows.Err(); err != nil {
		return m, err
	}
	m.Messages, err = s.negotiationTimeline(ctx, workspaceID, id)
	return m, err
}

// RevokeNegotiationMandate ends a mandate at once and gives up its deal.
func (s *DualTokenStore) RevokeNegotiationMandate(ctx context.Context, workspaceID, id, by string) (NegotiationMandate, error) {
	if _, err := s.pool.Exec(ctx, `UPDATE negotiation_mandates SET ended_at = now(), ended_by = $3
		WHERE id = $1 AND workspace_id = $2 AND ended_at IS NULL`, id, workspaceID, by); err != nil {
		return NegotiationMandate{}, fmt.Errorf("economy: revoke negotiation mandate: %w", err)
	}
	return s.NegotiationMandate(ctx, workspaceID, id)
}

// signedNegotiation reads mandate id of workspaceID and checks its signature again: what is answered is exactly what
// the owner signed, live, or an error.
func (s *DualTokenStore) signedNegotiation(ctx context.Context, workspaceID, id string) (NegotiationMandateTerms, error) {
	var canonical string
	var a passkey.Assertion
	var spki, deviceSPKI []byte
	var endedAt *time.Time
	err := s.pool.QueryRow(ctx, `SELECT m.canonical, m.credential_id, m.client_data_json, m.authenticator_data, m.signature, m.device_id,
		m.ended_at, k.public_key, d.public_key FROM negotiation_mandates m
		LEFT JOIN workspace_passkeys k ON k.credential_id = m.credential_id AND k.workspace_id = m.workspace_id AND k.removed_at IS NULL
		LEFT JOIN workspace_device_keys d ON d.id = m.device_id AND d.workspace_id = m.workspace_id AND d.revoked_at IS NULL
		WHERE m.id = $1 AND m.workspace_id = $2`, id, workspaceID).
		Scan(&canonical, &a.CredentialID, &a.ClientDataJSON, &a.AuthenticatorData, &a.Signature, &a.DeviceID, &endedAt, &spki, &deviceSPKI)
	if errors.Is(err, pgx.ErrNoRows) {
		return NegotiationMandateTerms{}, ErrNegotiationNotFound
	}
	if err != nil {
		return NegotiationMandateTerms{}, fmt.Errorf("economy: negotiation mandate: %w", err)
	}
	notSigned := fmt.Errorf("%w: the mandate is not what the owner signed, so it allows nothing", ErrNegotiationMandate)
	t, err := parseNegotiationCanonical(canonical)
	if err != nil || t.ID != id || t.WorkspaceID != workspaceID {
		return t, notSigned
	}
	switch {
	case a.DeviceID != "":
		if deviceSPKI == nil || devicekey.Verify(deviceSPKI, MandateChallenge(canonical), a.Signature) != nil {
			return t, notSigned
		}
	case spki == nil:
		return t, notSigned
	default:
		if _, err := s.verifyMandateSignature(canonical, a, spki, 0); err != nil {
			return t, notSigned
		}
	}
	if endedAt != nil {
		return t, fmt.Errorf("%w: the mandate was revoked", ErrNegotiationMandate)
	}
	if !time.Now().Before(t.Deadline) {
		return t, fmt.Errorf("%w: the mandate's deadline has passed", ErrNegotiationMandate)
	}
	return t, nil
}

// RecordNegotiationTerms adds o to the term sheet of its counterparty under mandate id: theirs as received, ours only
// when the signed mandate allows it — a counterparty and channel it names, an authority above research, a price short
// of the walk-away, no outbound text naming the walk-away, and a deal that counterparty has not confirmed to another.
func (s *DualTokenStore) RecordNegotiationTerms(ctx context.Context, workspaceID, id string, o NegotiationOffer) (NegotiationOffer, error) {
	t, err := s.signedNegotiation(ctx, workspaceID, id)
	if err != nil {
		return NegotiationOffer{}, err
	}
	bad := func(format string, args ...any) (NegotiationOffer, error) {
		return NegotiationOffer{}, fmt.Errorf("%w: %s", ErrNegotiationMandate, fmt.Sprintf(format, args...))
	}
	o.Counterparty, o.Channel = strings.ToLower(strings.TrimSpace(o.Counterparty)), strings.ToLower(o.Channel)
	switch {
	case !slices.Contains(t.Counterparties, o.Counterparty):
		return bad("%q is not a counterparty the mandate allows", o.Counterparty)
	case !slices.Contains(t.Channels, o.Channel):
		return bad("the mandate does not allow the %q channel", o.Channel)
	case o.Terms.PriceMinor <= 0:
		return bad("terms.price_minor must be more than 0")
	}
	switch o.Party {
	case "theirs":
	case "ours":
		if t.Authority == AuthorityResearch {
			return bad("the mandate allows research only: Counter makes no offer under it")
		}
		if t.Side == NegotiationBuy && o.Terms.PriceMinor >= t.WalkAwayMinor || t.Side == NegotiationSell && o.Terms.PriceMinor <= t.WalkAwayMinor {
			return bad("the offer is outside the mandate")
		}
		exp := MoneyCurrencies[t.Currency]
		for _, text := range append([]string{o.Message, o.Terms.Renewal, o.Terms.PaymentTerms, o.Terms.SLA}, o.Terms.Extras...) {
			if MentionsAmount(text, t.WalkAwayMinor, exp) {
				return bad("the offer's text names the walk-away, which is never shown to a counterparty")
			}
		}
	default:
		return bad(`party must be "ours" or "theirs"`)
	}
	terms, _ := json.Marshal(o.Terms)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if o.Party == "ours" {
			if err := lockDeal(ctx, tx, t.Deal); err != nil {
				return err
			}
			if held, err := dealHeld(ctx, tx, workspaceID, t, []string{o.Counterparty}); err != nil || held {
				return cmp.Or(err, errHeldElsewhere)
			}
		}
		err := tx.QueryRow(ctx, `INSERT INTO negotiation_terms (mandate_id, workspace_id, counterparty, party, channel, terms, message)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at`, id, workspaceID, o.Counterparty, o.Party, o.Channel, string(terms), o.Message).
			Scan(&o.ID, &o.CreatedAt)
		if err != nil {
			return fmt.Errorf("economy: negotiation terms: %w", err)
		}
		return nil
	})
	if err != nil {
		return NegotiationOffer{}, err
	}
	return o, nil
}

// A written number: digits with grouping or decimal separators, and a k, m, thousand or million after it. Spaced
// runs are read both ways ("12 000" and "12", "000") so a space can hide nothing.
var (
	amountSpaced = regexp.MustCompile(`(?i)(\d(?:[\d.,'’\x{00a0}\x{202f} ]*\d)?)(\s*(k|m|thousand|million)\b)?`)
	amountTight  = regexp.MustCompile(`(?i)(\d(?:[\d.,'’]*\d)?)(\s*(k|m|thousand|million)\b)?`)
	notDigit     = regexp.MustCompile(`\D`)
)

// MentionsAmount reports whether text writes minor (in a currency of exp decimals) in any common form: 12000,
// 12,000.00, 12.000,00, 12 000, 12k, 12 thousand, or as the minor units themselves. It errs towards yes.
// shortcut: numbers spelt in words ("twelve thousand") are not read; B52.11's engine sets every number, so upgrade
// here if the model's wording is ever let near the figures.
func MentionsAmount(text string, minor int64, exp int) bool {
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil))
	want := big.NewRat(minor, 1)
	for _, re := range []*regexp.Regexp{amountSpaced, amountTight} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			run, mult := m[1], big.NewRat(1, 1)
			switch strings.ToLower(m[3]) {
			case "k", "thousand":
				mult = big.NewRat(1000, 1)
			case "m", "million":
				mult = big.NewRat(1000000, 1)
			}
			forms := []string{notDigit.ReplaceAllString(run, "")} // every separator groups
			if i := strings.LastIndexAny(run, ".,"); i >= 0 {     // the last separator is the decimal point
				forms = append(forms, notDigit.ReplaceAllString(run[:i], "")+"."+notDigit.ReplaceAllString(run[i+1:], ""))
			}
			for _, f := range forms {
				v, ok := new(big.Rat).SetString(f)
				if !ok {
					continue
				}
				if mult.Cmp(big.NewRat(1, 1)) == 0 && v.Cmp(want) == 0 {
					return true // the minor units themselves
				}
				if new(big.Rat).Mul(new(big.Rat).Mul(v, mult), scale).Cmp(want) == 0 {
					return true
				}
			}
		}
	}
	return false
}
