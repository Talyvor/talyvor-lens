package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/economy"
)

// B50.47 — A QUESTION RETRIED UNDER THE SAME IDEMPOTENCY-KEY AFTER ITS HOLD WAS GIVEN BACK IS CHARGED.
//
// With reservations on, a request carrying an Idempotency-Key takes its hold under an id derived from the key,
// the credential, the model and the prompt. A first try that served nothing has its hold released, and the
// released row kept the id: a retry under the same key found it, was served as a replay, and its settle found
// nothing held and charged nothing. A migrated schema, the real handler. Asserted on lxc_ledger and
// agent_postings.

func b5047Setup(t *testing.T) (*Proxy, *economy.DualTokenStore, *pgxpool.Pool, string) {
	t.Helper()
	p, store, pool, agentID := b2313Setup(t, false, 10000, 100, nil)
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	return p, store, pool, agentID
}

// The first try is refused by the provider and its hold given back; the retry under the same key is served and
// charged for its answer, once.
func TestB5047_ARetryAfterAReleasedHoldIsChargedForItsAnswer(t *testing.T) {
	delivered := settleULXC(alerts.CostUSD("gpt-4o", 10000, 100))
	p, _, pool, agentID := b5047Setup(t)
	served := p.openAIURL
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"unknown parameter"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(refusing.Close)
	p.openAIURL = refusing.URL
	if w := b583AskWith(t, p, "first-try", false, "retry-me"); w.Code == http.StatusOK {
		t.Fatalf("status 200; the upstream was meant to refuse: %s", w.Body.String())
	}
	p.openAIURL = served
	if w := b583AskWith(t, p, "second-try", false, "retry-me"); w.Code != http.StatusOK {
		t.Fatalf("the retry: status %d, want 200; body=%s", w.Code, w.Body.String())
	}
	_, net, spent, _ := b2313Books(t, pool, agentID)
	if net != -delivered || spent != delivered {
		t.Errorf("one answer: ledger net %d, agent spent %d — want −%d and %d", net, spent, delivered, delivered)
	}
}

// Two tries under one key at once: the second is served on the first's hold. The first's failure does not give
// that hold back under the answer the second is giving; the second's settle charges it, once. A replayed hold
// both tries failed on is given back by the sweeper.
func TestB5047_AHoldARetryIsServedOnIsNotGivenBackUnderIt(t *testing.T) {
	ctx := context.Background()
	const held, final = int64(5_000_000), int64(1_200_000)
	reserveTwice := func(t *testing.T, store *economy.DualTokenStore, id string) {
		t.Helper()
		for range 2 {
			if err := store.ReserveLXCForAgent(ctx, b2313Key, b2313WS, id, held, economy.AgentDebitMeta{RequestedModel: "gpt-4o"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.ReleaseLXCReservation(ctx, id, "upstream error"); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("the retry answers", func(t *testing.T) {
		_, store, pool, agentID := b5047Setup(t)
		reserveTwice(t, store, "b5047-together")
		if _, _, err := store.SettleLXCReservation(ctx, "b5047-together", final, economy.AgentDebitMeta{ServedModel: "gpt-4o"}); err != nil {
			t.Fatal(err)
		}
		_, net, spent, _ := b2313Books(t, pool, agentID)
		if net != -final || spent != final {
			t.Errorf("one answer: ledger net %d, agent spent %d — want −%d and %d", net, spent, final, final)
		}
	})

	t.Run("both tries fail", func(t *testing.T) {
		_, store, pool, agentID := b5047Setup(t)
		reserveTwice(t, store, "b5047-both-failed")
		if err := store.ReleaseLXCReservation(ctx, "b5047-both-failed", "upstream error"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReleaseStrandedReservations(ctx, 0); err != nil {
			t.Fatal(err)
		}
		_, net, spent, holds := b2313Books(t, pool, agentID)
		if net != 0 || spent != 0 || holds != b2313Fund {
			t.Errorf("no answer: ledger net %d, agent spent %d and holds %d — want 0, 0 and %d", net, spent, holds, b2313Fund)
		}
	})
}

// A retry that found the hold just before a release took it is never served on nothing: the release frees the id
// under it, and the retry takes a new hold.
func TestB5047_ARetryThatLosesItsHoldToAReleaseTakesANewOne(t *testing.T) {
	ctx := context.Background()
	const id, held = "b5047-raced", int64(5_000_000)
	_, store, pool, _ := b5047Setup(t)
	meta := economy.AgentDebitMeta{RequestedModel: "gpt-4o"}
	if err := store.ReserveLXCForAgent(ctx, b2313Key, b2313WS, id, held, meta); err != nil {
		t.Fatal(err)
	}
	// The release holds the row; the retry finds it held and waits to count itself in.
	release, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release.Rollback(ctx) }()
	if _, err := release.Exec(ctx, `SELECT 1 FROM lxc_reservations WHERE reservation_id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	retried := make(chan error, 1)
	go func() { retried <- store.ReserveLXCForAgent(ctx, b2313Key, b2313WS, id, held, meta) }()
	for waiting := 0; waiting == 0; {
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := release.Exec(ctx, `UPDATE lxc_reservations SET status = 'released', resolved_at = now(),
		reservation_id = reservation_id || ':released:test' WHERE reservation_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := release.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-retried; err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM lxc_reservations WHERE reservation_id = $1`, id).Scan(&status); err != nil || status != "held" {
		t.Errorf("the retry's hold: status %q (%v), want a new hold — it was served on none", status, err)
	}
}
