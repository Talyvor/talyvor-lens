package economy

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// negotiation_outreach.go — B52.12: COUNTER REACHING THE OTHER SIDE, HONESTLY.
//
// SendNegotiationOutreach sends the engine's next move (B52.11) to a counterparty on a channel the signed mandate
// allows. The first message to each counterparty says it is from an AI agent acting for the owner, by the name the
// owner's verification confirmed (EU AI Act Art. 50, California's BOT Act), and every message says the owner reads
// every reply and can be dealt with directly. Every message goes on the deal's timeline.
//
//   - agent: to a Talyvor agent with a live Know Your Agent credential, in another workspace. It lands in that
//     workspace's negotiation inbox, and its reply threads into the deal of the message it answers.
//   - web_form: to a form on a site the mandate names and the refused-sites list does not. Lens fills no form: it
//     answers the text, and the owner's agent submits it there.
//   - email: refused until Lens has a sender for Counter's address (B52.39).
//
// Nothing here moves money. Class: rules_approvals_statements_pots, GREEN.

// ErrNegotiationChannel: Counter does not reach this counterparty this way — a refused site, an agent that is not
// verified, or a channel Lens cannot send on.
var ErrNegotiationChannel = errors.New("economy: Counter cannot reach the counterparty this way")

// counterRefusedSites are sites whose terms ban buying agents, matched as any label of a form's host, so every country
// domain and subdomain is refused too. eBay has banned unapproved buy-for-me agents since 20 Feb 2026.
var counterRefusedSites = []string{"ebay"}

// CounterSiteRefused reports whether Counter may never fill a form on host.
func CounterSiteRefused(host string) bool {
	for _, label := range strings.Split(strings.ToLower(strings.TrimSuffix(host, ".")), ".") {
		if slices.Contains(counterRefusedSites, label) {
			return true
		}
	}
	return false
}

// NegotiationMessage is one message on a deal's timeline: Counter's out, or a counterparty's reply in.
type NegotiationMessage struct {
	ID           int64     `json:"id"`
	Counterparty string    `json:"counterparty"`
	Channel      string    `json:"channel"`
	Direction    string    `json:"direction"` // out or in
	Address      string    `json:"address"`   // the agent's id or the form's URL
	Body         string    `json:"body"`
	CreatedAt    time.Time `json:"created_at"`
}

// NegotiationOutreach is the engine's move with a counterparty and, when it has words, the message sent.
type NegotiationOutreach struct {
	Move     NegotiationMove     `json:"move"`
	Sent     *NegotiationMessage `json:"sent,omitempty"`
	Delivery string              `json:"delivery,omitempty"` // delivered (to the agent's inbox) or submit_form
}

// SendNegotiationOutreach sends the engine's next move with counterparty under mandate id on channel, to the agent id
// or form URL `to`.
func (s *DualTokenStore) SendNegotiationOutreach(ctx context.Context, workspaceID, id, counterparty, channel, to string,
	benchmarks []int64, mentionRival bool, ai NegotiationInferer, model string) (NegotiationOutreach, error) {
	t, err := s.signedNegotiation(ctx, workspaceID, id)
	if err != nil {
		return NegotiationOutreach{}, err
	}
	counterparty, channel, to = strings.ToLower(strings.TrimSpace(counterparty)), strings.ToLower(channel), strings.TrimSpace(to)
	refuse := func(format string, args ...any) (NegotiationOutreach, error) {
		return NegotiationOutreach{}, fmt.Errorf("%w: %s", ErrNegotiationChannel, fmt.Sprintf(format, args...))
	}
	switch {
	case !slices.Contains(t.Counterparties, counterparty):
		return NegotiationOutreach{}, fmt.Errorf("%w: %q is not a counterparty the mandate allows", ErrNegotiationMandate, counterparty)
	case !slices.Contains(t.Channels, channel):
		return NegotiationOutreach{}, fmt.Errorf("%w: the mandate does not allow the %q channel", ErrNegotiationMandate, channel)
	}
	out := NegotiationOutreach{}
	var toWorkspace string
	switch channel {
	case ChannelAgent:
		err := s.pool.QueryRow(ctx, `SELECT workspace_id FROM agent_accounts WHERE id = $1 AND archived_at IS NULL`, to).Scan(&toWorkspace)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("no Talyvor agent has the id %q", to)
		}
		if err != nil {
			return NegotiationOutreach{}, fmt.Errorf("economy: negotiation outreach: %w", err)
		}
		if toWorkspace == workspaceID {
			return refuse("the agent is one of yours, and Counter acts for one side of a deal")
		}
		if _, ok, err := s.LatestKYACredential(ctx, toWorkspace, to, time.Now()); err != nil {
			return NegotiationOutreach{}, err
		} else if !ok {
			return refuse("the agent has no live Know Your Agent credential, and Counter messages verified agents only")
		}
		out.Delivery = "delivered"
	case ChannelWebForm:
		u, err := url.Parse(to)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			return refuse("a web form is named by its https URL")
		}
		host := strings.ToLower(u.Hostname())
		if CounterSiteRefused(host) {
			return refuse("%s bans buying agents, so Counter never contacts it", host)
		}
		if !slices.ContainsFunc(t.WebForms, func(site string) bool { return host == site || strings.HasSuffix(host, "."+site) }) {
			return refuse("the mandate does not name %s among the sites whose forms Counter may fill", host)
		}
		out.Delivery = "submit_form"
	default:
		return refuse("Counter has no sending address for email yet, so it sends none")
	}
	owner, err := kyaOwner(ctx, s.pool, workspaceID)
	if err != nil {
		return NegotiationOutreach{}, err
	}
	if owner.Name == "" {
		return refuse("Counter tells every counterparty whom it acts for, and this workspace has no verified name yet: complete a verification first")
	}
	var contacted bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM negotiation_messages WHERE mandate_id = $1 AND counterparty = $2
		AND direction = 'out')`, id, counterparty).Scan(&contacted); err != nil {
		return NegotiationOutreach{}, fmt.Errorf("economy: negotiation outreach: %w", err)
	}
	// shortcut: a counter is on the term sheet before its message is stored, so a failed insert leaves it unsent and
	// the next move waits; move both into one transaction if that is ever seen.
	if out.Move, err = s.NextNegotiationMove(ctx, workspaceID, id, counterparty, channel, benchmarks, mentionRival, ai, model); err != nil {
		return out, err
	}
	if out.Move.Message == "" {
		out.Delivery = ""
		return out, nil
	}
	body := out.Move.Message + "\n\n" + owner.Name + " reads every reply. To deal with them directly, say so in your reply."
	if !contacted {
		body = "This message is from Talyvor Counter, an AI agent negotiating on behalf of " + owner.Name + ".\n\n" + body
	}
	if MentionsAmount(body, t.WalkAwayMinor, MoneyCurrencies[t.Currency]) {
		return out, fmt.Errorf("%w: the message would name the walk-away, which is never shown to a counterparty", ErrNegotiationMandate)
	}
	m := NegotiationMessage{Counterparty: counterparty, Channel: channel, Direction: "out", Address: to, Body: body}
	if err := s.pool.QueryRow(ctx, `INSERT INTO negotiation_messages (mandate_id, workspace_id, counterparty, channel, direction,
		address, to_workspace_id, body) VALUES ($1, $2, $3, $4, 'out', $5, $6, $7) RETURNING id, created_at`,
		id, workspaceID, counterparty, channel, to, toWorkspace, body).Scan(&m.ID, &m.CreatedAt); err != nil {
		return out, fmt.Errorf("economy: negotiation outreach: %w", err)
	}
	out.Sent = &m
	return out, nil
}

// NegotiationInboxMessage is a message Counter sent to one of a workspace's agents. It carries nothing of the deal
// but the message itself.
type NegotiationInboxMessage struct {
	ID        int64     `json:"id"`
	AgentID   string    `json:"agent_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// NegotiationInbox is the newest 100 messages Counter sent to workspaceID's agents.
func (s *DualTokenStore) NegotiationInbox(ctx context.Context, workspaceID string) ([]NegotiationInboxMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, address, body, created_at FROM negotiation_messages
		WHERE to_workspace_id = $1 AND direction = 'out' ORDER BY id DESC LIMIT 100`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: negotiation inbox: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (NegotiationInboxMessage, error) {
		var m NegotiationInboxMessage
		return m, r.Scan(&m.ID, &m.AgentID, &m.Body, &m.CreatedAt)
	})
}

// maxNegotiationReply bounds a counterparty's reply.
const maxNegotiationReply = 20000

// ReplyToNegotiationMessage threads workspaceID's reply to message id, one Counter sent to one of its agents, into
// that message's deal, for the owner to read. It reaches the owner even after the mandate ends.
func (s *DualTokenStore) ReplyToNegotiationMessage(ctx context.Context, workspaceID string, id int64, body string) (NegotiationMessage, error) {
	body = strings.TrimSpace(body)
	if body == "" || len(body) > maxNegotiationReply {
		return NegotiationMessage{}, fmt.Errorf("%w: a reply is 1 to %d characters", ErrNegotiationMandate, maxNegotiationReply)
	}
	m := NegotiationMessage{Direction: "in", Body: body}
	err := s.pool.QueryRow(ctx, `INSERT INTO negotiation_messages (mandate_id, workspace_id, counterparty, channel, direction, address, body)
		SELECT mandate_id, workspace_id, counterparty, channel, 'in', address, $3::text FROM negotiation_messages
		WHERE id = $1 AND to_workspace_id = $2 AND direction = 'out'
		RETURNING id, counterparty, channel, address, created_at`, id, workspaceID, body).
		Scan(&m.ID, &m.Counterparty, &m.Channel, &m.Address, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNegotiationNotFound
	}
	if err != nil {
		return m, fmt.Errorf("economy: negotiation reply: %w", err)
	}
	return m, nil
}

// negotiationTimeline is mandate id's messages, oldest first.
func (s *DualTokenStore) negotiationTimeline(ctx context.Context, workspaceID, id string) ([]NegotiationMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, counterparty, channel, direction, address, body, created_at FROM negotiation_messages
		WHERE mandate_id = $1 AND workspace_id = $2 ORDER BY id`, id, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("economy: negotiation timeline: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (NegotiationMessage, error) {
		var m NegotiationMessage
		return m, r.Scan(&m.ID, &m.Counterparty, &m.Channel, &m.Direction, &m.Address, &m.Body, &m.CreatedAt)
	})
}
