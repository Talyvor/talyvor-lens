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
