package learner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/pashagolub/pgxmock/v4"
)

// runEmbeddedNATS starts an in-process NATS server with JetStream enabled
// and returns a connected client. The server shuts down at test cleanup.
func runEmbeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // ephemeral
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("natsserver.NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready in time")
	}

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		srv.Shutdown()
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(func() {
		nc.Close()
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	return nc
}

func newTestLearner(t *testing.T) (*Learner, *nats.Conn, pgxmock.PgxPoolIface) {
	t.Helper()
	nc := runEmbeddedNATS(t)

	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)

	l, err := newLearner(nc, pool)
	if err != nil {
		t.Fatalf("newLearner: %v", err)
	}
	return l, nc, pool
}

func TestLearner_RecordPublishesToCorrectSubject(t *testing.T) {
	l, nc, _ := newTestLearner(t)

	received := make(chan *nats.Msg, 1)
	sub, err := nc.SubscribeSync("lens.events.token")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	ev := TokenEvent{
		Provider:     "openai",
		Model:        "gpt-4o-mini",
		Prompt:       "hello",
		Response:     "hi",
		InputTokens:  10,
		OutputTokens: 5,
		Timestamp:    time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
	}
	if err := l.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}

	go func() {
		msg, _ := sub.NextMsg(2 * time.Second)
		received <- msg
	}()

	select {
	case msg := <-received:
		if msg == nil {
			t.Fatal("no message received on lens.events.token")
		}
		var got TokenEvent
		if err := json.Unmarshal(msg.Data, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Provider != "openai" || got.Model != "gpt-4o-mini" || got.Prompt != "hello" {
			t.Errorf("unexpected event payload: %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for NATS message")
	}
}

func TestLearner_RecordNeverErrorsEvenWhenPublishFails(t *testing.T) {
	l, nc, _ := newTestLearner(t)

	// Force publish to fail by closing the connection before Record runs.
	nc.Close()

	err := l.Record(context.Background(), TokenEvent{
		Provider: "openai",
		Model:    "gpt-4o-mini",
		Prompt:   "hello",
	})
	if err != nil {
		t.Fatalf("Record returned error on closed conn: %v", err)
	}
}

func TestLearner_AnalyseReturnsInsightsSortedByHitCountDesc(t *testing.T) {
	l, _, pool := newTestLearner(t)

	rows := pgxmock.NewRows([]string{"prompt_hash", "hit_count", "avg_tokens", "last_seen"}).
		AddRow("hash-low", 3, float64(120), time.Now()).
		AddRow("hash-high", 10, float64(200), time.Now()).
		AddRow("hash-mid", 5, float64(150), time.Now())

	pool.ExpectQuery(`SELECT prompt_hash`).WithArgs("").WillReturnRows(rows)

	insights, err := l.Analyse(context.Background())
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}

	if len(insights) != 3 {
		t.Fatalf("got %d insights, want 3", len(insights))
	}
	if insights[0].HitCount < insights[1].HitCount || insights[1].HitCount < insights[2].HitCount {
		t.Errorf("insights not sorted by HitCount desc: %+v", insights)
	}
	if insights[0].PromptPattern != "hash-high" {
		t.Errorf("top pattern = %q, want %q", insights[0].PromptPattern, "hash-high")
	}
	if insights[0].Recommendation == "" {
		t.Error("Recommendation should not be empty")
	}
}

func TestLearner_AnalyseQueryFiltersPatternsSeenLessThanThree(t *testing.T) {
	l, _, pool := newTestLearner(t)

	// pgxmock matches the expected SQL as a regex. Requiring the HAVING clause
	// here means the test fails if Analyse ever emits SQL without the >=3 filter.
	pool.ExpectQuery(`HAVING COUNT\(\*\) >= 3`).WithArgs("").
		WillReturnRows(pgxmock.NewRows([]string{"prompt_hash", "hit_count", "avg_tokens", "last_seen"}))

	if _, err := l.Analyse(context.Background()); err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations (SQL missing HAVING COUNT(*) >= 3?): %v", err)
	}
}

func TestLearner_StartBackgroundStopsOnContextCancel(t *testing.T) {
	l, _, _ := newTestLearner(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.runLoop(ctx, time.Hour) // long interval so we only exit via cancel
		close(done)
	}()

	// Give the loop a moment to enter the select.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runLoop did not exit within 2s after context cancel")
	}
}
