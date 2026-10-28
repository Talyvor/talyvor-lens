package main

import (
	"bufio"
	"bytes"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B50.2: API keys, tokens, passwords, IBANs and card numbers never reach Lens's logs — in a message, an
// attribute, an error, or a line from the standard log package — and a CR/LF in logged text cannot forge a line.
func TestLogsNeverCarrySecretsOrForgedLines(t *testing.T) {
	var buf bytes.Buffer
	logger := newLoggerTo(&buf, "debug")
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	secrets := []string{
		"tlv_ws_9f8e7d6c5b4a39281706",                                   // a workspace key
		"sk_live_51Habcdefghijklmnop",                                   // a Stripe key
		"whsec_test_abcdefgh12345678",                                   // a webhook secret
		"sk-proj-abcdefghijklmnop1234",                                  // a provider key
		"eyJhbGciOiJFZERTQSJ9.eyJzdWIiOiJ3cy0xIn0.c2lnbmF0dXJlLXZhbHVl", // a session token
		"hunter2-correct-horse",                                         // a password
		"GB82WEST12345698765432",                                        // an IBAN
		"DE89 3704 0044 0532 0130 00",                                   // an IBAN, spaced
		"4242424242424242",                                              // a card number
		"5555-5555-5555-4444",                                           // a card number, dashed
	}
	logger.Info("calling upstream with Authorization: Bearer "+secrets[0], slog.String("stripe", secrets[1]))
	logger.Warn("webhook", slog.Any("err", errors.New("bad signature for "+secrets[2])), slog.String("note", "key "+secrets[3]))
	logger.Info("login", slog.String("session_token", secrets[4]), slog.String("password", secrets[5]))
	logger.Info("payout to "+secrets[6]+" and "+secrets[7], slog.String("card", "paid with "+secrets[8]))
	log.Printf("legacy line: card %s", secrets[9])
	logger.Info("ledger", slog.Int64("amount_minor", 4111111111111111), slog.String("id", "1700000000000000001"), slog.Int("input_tokens", 913))
	logger.Info("user said: hi\r\n{\"level\":\"ERROR\",\"msg\":\"forged\"}")

	out := buf.String()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("the logs carry %q:\n%s", s, out)
		}
	}
	if !strings.Contains(out, "4111111111111111") || !strings.Contains(out, "1700000000000000001") || !strings.Contains(out, `"input_tokens":913`) {
		t.Errorf("a number logged as a number, or an id failing the card checksum, was cut:\n%s", out)
	}
	lines := 0
	for sc := bufio.NewScanner(strings.NewReader(out)); sc.Scan(); lines++ {
		if strings.HasPrefix(sc.Text(), `{"level":"ERROR","msg":"forged"`) {
			t.Errorf("a CR/LF in logged text forged a line: %s", sc.Text())
		}
	}
	if lines != 7 {
		t.Errorf("7 records wrote %d lines", lines)
	}
}

// B50.2: a CR/LF in a value Lens sets as a response header cannot add a header.
func TestResponseHeaderCannotBeSplit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="`+r.URL.Query().Get("name")+`"`)
	}))
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("GET /?name=a%0d%0aSet-Cookie:%20session=evil HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"))
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(conn)
	if strings.Contains(strings.ToLower(raw.String()), "\r\nset-cookie:") {
		t.Fatalf("a CR/LF in a header value added a header:\n%s", raw.String())
	}
}
