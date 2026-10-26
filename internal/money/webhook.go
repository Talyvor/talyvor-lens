package money

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrBadSignature: a webhook whose signature does not verify, or that is too old to be taken.
var ErrBadSignature = errors.New("money: the webhook's signature does not verify")

// ErrNotACredit: a notification whose signature verified but that brings no money in — a payment going out, a state
// change, a token Talyvor does not hold. It is answered 200 so the partner stops sending it, and nothing is posted.
var ErrNotACredit = errors.New("money: the notification brings no money in")

// Event is a partner's notification that money came into an account or a wallet, once its signature has verified.
type Event struct {
	ID         string `json:"id"`          // the partner's id for the notification: it is taken once
	To         string `json:"to"`          // the partner's reference for the account or wallet the money came into
	Amount     Amount `json:"amount"`      //
	PartnerRef string `json:"partner_ref"` // the partner's reference for the payment: it posts once
	Payer      string `json:"payer"`
	Reference  string `json:"reference,omitempty"` // what the payer quoted
}

// Webhooks verifies a partner's notification and reads it into an Event: ErrBadSignature when it does not verify, and
// ErrNotACredit — only once it has — when it brings no money in.
type Webhooks interface {
	Verify(h http.Header, body []byte, now time.Time) (Event, error)
}

// Ledger posts the money a verified webhook reports, and answers the workspace it went to.
type Ledger interface {
	PostCredit(ctx context.Context, partner string, e Event) (workspaceID string, err error)
}

// Replays is the webhook ids already taken, per partner.
type Replays interface {
	Taken(ctx context.Context, partner, id string) (bool, error)
	Take(ctx context.Context, partner, id string) error
}

// ServeWebhook is POST /v1/money/webhooks/{partner}: one endpoint per adapter, and the one path a partner's
// notification becomes a ledger posting by. The adapter verifies the signature (401 when it does not); an id already
// taken is refused (409); the money is posted, the webhook is linked into the audit trail, and its id is taken last, so
// a notification whose posting failed is posted when the partner sends it again.
func (r *Registry) ServeWebhook(w http.ResponseWriter, req *http.Request) {
	partner := chi.URLParam(req, "partner")
	a := r.adapter(partner)
	if a == nil || a.Webhooks == nil {
		http.Error(w, "no such partner", http.StatusNotFound)
		return
	}
	if r.cfg.Ledger == nil || r.cfg.Replays == nil {
		http.Error(w, "money webhooks are not set up", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	ctx := req.Context()
	e, err := a.Webhooks.Verify(req.Header, body, time.Now())
	if errors.Is(err, ErrNotACredit) {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		slog.Warn("money: a webhook was refused", slog.String("partner", partner), slog.String("err", err.Error()))
		if errors.Is(err, ErrBadSignature) {
			http.Error(w, "signature does not verify", http.StatusUnauthorized)
		} else {
			http.Error(w, "the notification is unreadable", http.StatusBadRequest)
		}
		return
	}
	if e.ID == "" || e.To == "" || e.PartnerRef == "" || e.Amount.check() != nil {
		http.Error(w, "the notification is incomplete", http.StatusBadRequest)
		return
	}
	taken, err := r.cfg.Replays.Taken(ctx, partner, e.ID)
	if err != nil {
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	if taken {
		http.Error(w, "this notification was already taken", http.StatusConflict)
		return
	}
	ws, err := r.cfg.Ledger.PostCredit(ctx, partner, e)
	if err != nil {
		slog.Warn("money: a webhook's money was not posted", slog.String("partner", partner), slog.String("id", e.ID), slog.String("err", err.Error()))
		http.Error(w, "not posted", http.StatusUnprocessableEntity)
		return
	}
	if err := r.cfg.Audit.AppendAudit(ctx, ws, "money_webhook:"+partner+":"+e.ID, "money.webhook.credit", map[string]any{
		"partner": partner, "event_id": e.ID, "to": e.To, "partner_ref": e.PartnerRef, "amount_minor": e.Amount.Minor, "asset": e.Amount.Asset,
	}); err != nil {
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	if err := r.cfg.Replays.Take(ctx, partner, e.ID); err != nil {
		slog.Warn("money: a webhook id was not kept", slog.String("partner", partner), slog.String("id", e.ID), slog.String("err", err.Error()))
	}
	w.WriteHeader(http.StatusOK)
}

// ReplayStore is Replays in money_webhook_events (migration 0270).
type ReplayStore struct{ pool *pgxpool.Pool }

// NewReplayStore is the taken webhook ids in pool.
func NewReplayStore(pool *pgxpool.Pool) *ReplayStore { return &ReplayStore{pool: pool} }

// Taken is whether partner's id was taken.
func (s *ReplayStore) Taken(ctx context.Context, partner, id string) (bool, error) {
	var taken bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM money_webhook_events WHERE partner = $1 AND event_id = $2)`, partner, id).Scan(&taken)
	return taken, err
}

// Take keeps partner's id; taking it again changes nothing.
func (s *ReplayStore) Take(ctx context.Context, partner, id string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO money_webhook_events (partner, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, partner, id)
	return err
}

// SandboxSignatureHeader carries the sandbox's webhook signature: "t=<unix seconds>,v1=<hex HMAC-SHA256 of t.body>".
const SandboxSignatureHeader = "Talyvor-Sandbox-Signature"

// signatureTolerance is how old a signed webhook may be.
const signatureTolerance = 5 * time.Minute

// HMACWebhooks verifies webhooks signed as the sandbox signs them, with Secret. An empty Secret verifies none.
type HMACWebhooks struct {
	Secret string
}

// SignSandbox is the SandboxSignatureHeader value for body at t.
func SignSandbox(secret string, body []byte, t time.Time) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(sign(secret, ts, body))
}

func sign(secret, ts string, body []byte) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return m.Sum(nil)
}

// Verify checks the signature and its age, and reads the body as an Event.
func (h HMACWebhooks) Verify(hd http.Header, body []byte, now time.Time) (Event, error) {
	if h.Secret == "" {
		return Event{}, fmt.Errorf("%w: no webhook secret is set", ErrBadSignature)
	}
	var ts, v1 string
	for _, part := range strings.Split(hd.Get(SandboxSignatureHeader), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			v1 = v
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return Event{}, fmt.Errorf("%w: no timestamp", ErrBadSignature)
	}
	if age := now.Sub(time.Unix(sec, 0)); age > signatureTolerance || age < -signatureTolerance {
		return Event{}, fmt.Errorf("%w: signed %s from now", ErrBadSignature, age.Round(time.Second))
	}
	got, err := hex.DecodeString(v1)
	if err != nil || !hmac.Equal(got, sign(h.Secret, ts, body)) {
		return Event{}, ErrBadSignature
	}
	var e Event
	if err := json.Unmarshal(body, &e); err != nil {
		return Event{}, fmt.Errorf("%w: the body is not a notification: %v", ErrInvalid, err)
	}
	return e, nil
}
