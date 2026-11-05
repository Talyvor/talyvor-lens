package economy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
// any way a number is written, and an offer that reaches it. One side per deal: a deal holds at most one live mandate
// across every workspace, so Counter never acts for both sides of one deal or for two bidders in one RFQ, and the
// refusal names nobody. Nothing here moves money. Class: rules_approvals_statements_pots, GREEN.

var (
	// ErrNegotiationMandate: terms a negotiation mandate cannot have, a signature that does not sign them, or an
	// offer outside them.
	ErrNegotiationMandate = errors.New("economy: invalid negotiation mandate")
	// ErrNegotiationNotFound: no such negotiation in this workspace.
	ErrNegotiationNotFound = errors.New("economy: no such negotiation")
	// ErrNegotiationOneSide: Counter already acts for a party in this deal.
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
	Status       string                        `json:"status"` // active, past_deadline or revoked
	Canonical    string                        `json:"canonical"`
	CredentialID string                        `json:"credential_id,omitempty"`
	DeviceID     string                        `json:"device_id,omitempty"`
	CreatedBy    string                        `json:"created_by"`
	CreatedAt    time.Time                     `json:"created_at"`
	EndedAt      *time.Time                    `json:"ended_at,omitempty"`
	TermSheets   map[string][]NegotiationOffer `json:"term_sheets"`
	Messages     []NegotiationMessage          `json:"messages"` // the deal's timeline: what Counter sent and the replies
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
// workspace's, one already kept, or one in a deal Counter already acts in is refused.
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
		if a.DeviceID != "" {
			if err := signDevice(ctx, tx, workspaceID, MandateChallenge(canonical), *a); err != nil {
				return err
			}
			a = &passkey.Assertion{DeviceID: a.DeviceID, Signature: a.Signature}
		} else {
			var spki []byte
			var count int64
			err := tx.QueryRow(ctx, `SELECT public_key, sign_count FROM workspace_passkeys WHERE credential_id = $1 AND workspace_id = $2 FOR UPDATE`,
				a.CredentialID, workspaceID).Scan(&spki, &count)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: not a passkey of this workspace", passkey.ErrInvalid)
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
		if t.Deal != "" {
			// A mandate past its deadline gives its deal up; a live one keeps it, whoever's it is.
			if _, err := tx.Exec(ctx, `UPDATE negotiation_mandates SET ended_at = now(), ended_by = 'deadline'
				WHERE deal_ref = $1 AND ended_at IS NULL AND deadline <= now()`, t.Deal); err != nil {
				return fmt.Errorf("economy: negotiation mandate: %w", err)
			}
			var holder string
			err := tx.QueryRow(ctx, `SELECT workspace_id FROM negotiation_mandates WHERE deal_ref = $1 AND ended_at IS NULL`, t.Deal).Scan(&holder)
			switch {
			case err == nil && holder == workspaceID:
				return fmt.Errorf("%w: you already have a live mandate in this deal: revoke it first", ErrNegotiationOneSide)
			case err == nil:
				return errOtherParty
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("economy: negotiation mandate: %w", err)
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO negotiation_mandates (id, workspace_id, deal_ref, deadline, canonical, credential_id, device_id,
			client_data_json, authenticator_data, signature, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			t.ID, workspaceID, t.Deal, t.Deadline, canonical, a.CredentialID, a.DeviceID, a.ClientDataJSON, a.AuthenticatorData, a.Signature, by)
		if pe := (*pgconn.PgError)(nil); errors.As(err, &pe) && pe.Code == "23505" {
			if pe.ConstraintName == "idx_negotiation_mandates_live_deal" {
				return errOtherParty
			}
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

// errOtherParty names nobody: who else Counter acts for, and on what terms, is theirs.
var errOtherParty = fmt.Errorf("%w: Counter already acts for another party in this deal, and represents only one side of a deal and one bidder in an RFQ or auction", ErrNegotiationOneSide)

// NegotiationMandate reads one of workspaceID's mandates, with its term sheets.
func (s *DualTokenStore) NegotiationMandate(ctx context.Context, workspaceID, id string) (NegotiationMandate, error) {
	var m NegotiationMandate
	err := s.pool.QueryRow(ctx, `SELECT canonical, credential_id, device_id, created_by, created_at, ended_at FROM negotiation_mandates
		WHERE id = $1 AND workspace_id = $2`, id, workspaceID).Scan(&m.Canonical, &m.CredentialID, &m.DeviceID, &m.CreatedBy, &m.CreatedAt, &m.EndedAt)
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
		LEFT JOIN workspace_passkeys k ON k.credential_id = m.credential_id AND k.workspace_id = m.workspace_id
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
// of the walk-away, and no outbound text naming the walk-away.
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
	err = s.pool.QueryRow(ctx, `INSERT INTO negotiation_terms (mandate_id, workspace_id, counterparty, party, channel, terms, message)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at`, id, workspaceID, o.Counterparty, o.Party, o.Channel, string(terms), o.Message).
		Scan(&o.ID, &o.CreatedAt)
	if err != nil {
		return NegotiationOffer{}, fmt.Errorf("economy: negotiation terms: %w", err)
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
