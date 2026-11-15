package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/lens/internal/dbmigrate"
	"github.com/talyvor/lens/internal/partners"
	"github.com/talyvor/lens/internal/phonesignin"
	"github.com/talyvor/lens/migrations"
)

// B61.22 — the phone door's routes, mounted as main.go mounts them, on a migrated schema.

func phoneRoutesDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("LENS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LENS_TEST_DATABASE_URL not set — skipping real-PG phone door routes test")
	}
	const schema = "cmd_phone_signin_realpg"
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`, `CREATE SCHEMA ` + schema} {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			t.Fatalf("reset schema: %v", err)
		}
	}
	if _, err := dbmigrate.Run(ctx, conn, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = conn.Close(ctx)
	poolCfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestPhoneDoorRoutes(t *testing.T) {
	const secret = "provision-secret"
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	reg := partners.NewRegistry(nil)
	clock := time.Now()
	store := &phonesignin.Store{DB: phoneRoutesDB(t), Sender: reg.TextMessages, Key: []byte(secret), PerAddressHour: 20,
		Now: func() time.Time { return clock }}
	r := chi.NewRouter()
	mountPhoneSignInRoutes(r, secret, store)
	call := func(path string, body any, auth string) (int, map[string]any) {
		t.Helper()
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
		if auth != "" {
			req.Header.Set(provisionSecretHeader, auth)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	if code, _ := call("/v1/auth/phone/start", map[string]string{"phone": "+447700900301", "caller_address": "203.0.113.30"}, ""); code != http.StatusUnauthorized {
		t.Fatalf("no secret: %d, want 401", code)
	}
	if code, out := call("/v1/auth/phone/start", map[string]string{"phone": "+800 1234 5678", "caller_address": "203.0.113.30"}, secret); code != http.StatusBadRequest ||
		!strings.Contains(out["error"].(string), "the country is an ISO 3166-1 alpha-2 code, like GB") {
		t.Fatalf("a number in no country: %d %v", code, out)
	}

	// A person for the known number.
	const known, unknown = "+447700900302", "+447700900303"
	_, st := call("/v1/auth/phone/start", map[string]string{"phone": known, "caller_address": "203.0.113.30"}, secret)
	code := st["sandbox_code"].(string)
	_, fin := call("/v1/auth/phone/finish", map[string]string{"phone": known, "code": code, "caller_address": "203.0.113.30"}, secret)
	status, person := call("/v1/auth/phone/person", map[string]string{"ticket": fin["ticket"].(string), "caller_address": "203.0.113.30"}, secret)
	if status != http.StatusOK || person["created"] != true {
		t.Fatalf("joining: %d %v", status, person)
	}
	if status, again := call("/v1/auth/phone/person", map[string]string{"ticket": fin["ticket"].(string), "caller_address": "203.0.113.30"}, secret); status != http.StatusBadRequest ||
		again["reason"] != "ticket_ended" {
		t.Fatalf("the ticket twice: %d %v", status, again)
	}
	codes := []string{code}

	// Until the code is right, a known and an unknown number get the same status and words.
	clock = clock.Add(time.Minute)
	answers := map[string][]string{}
	for _, phone := range []string{known, unknown} {
		status, st := call("/v1/auth/phone/start", map[string]string{"phone": phone, "caller_address": "203.0.113.31"}, secret)
		codes = append(codes, st["sandbox_code"].(string))
		delete(st, "expires_at")
		delete(st, "sandbox_code")
		b, _ := json.Marshal(st)
		answers[phone] = append(answers[phone], http.StatusText(status), string(b))
		wrong := "000000"
		if codes[len(codes)-1] == wrong {
			wrong = "000001"
		}
		status, out := call("/v1/auth/phone/finish", map[string]string{"phone": phone, "code": wrong, "caller_address": "203.0.113.31"}, secret)
		b, _ = json.Marshal(out)
		answers[phone] = append(answers[phone], http.StatusText(status), string(b))
	}
	if strings.Join(answers[known], " | ") != strings.Join(answers[unknown], " | ") {
		t.Fatalf("a known and an unknown number answered differently before the code:\n%v\n%v", answers[known], answers[unknown])
	}
	if !strings.Contains(strings.Join(answers[known], " "), "That code is not right") {
		t.Fatalf("the wrong code's words: %v", answers[known])
	}
	_, fin = call("/v1/auth/phone/finish", map[string]string{"phone": known, "code": codes[1], "caller_address": "203.0.113.31"}, secret)
	if fin["has_person"] != true {
		t.Fatalf("after the right code a known number says so: %v", fin)
	}

	// The sixth text inside the hour is refused with when to try again.
	limited := false
	for i := 0; i < 6 && !limited; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/auth/phone/start", strings.NewReader(`{"phone":"+447700900304","caller_address":"203.0.113.32"}`))
		req.Header.Set(provisionSecretHeader, secret)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited = rec.Header().Get("Retry-After") != "" && strings.Contains(rec.Body.String(), "retry_at")
		}
	}
	if !limited {
		t.Fatal("a second text inside 30 seconds must be refused with Retry-After and retry_at")
	}

	for _, c := range codes {
		if strings.Contains(logs.String(), c) {
			t.Fatalf("a code is in the logs: %s", logs.String())
		}
	}
}
