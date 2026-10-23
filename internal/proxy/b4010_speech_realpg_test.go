package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/workspace"
)

// B40.10 — speech through Lens. An agent sends a 10-second clip to /v1/audio/transcriptions with its own
// key; Deepgram (a fake answering with a fixture in Deepgram's response shape) hears 10 seconds; the agent
// is charged one line at nova-3's catalog price per minute, and nothing else moves.

// tenSecondWAV is 10 s of 8 kHz 16-bit mono silence with its RIFF header.
func tenSecondWAV() []byte {
	const rate, seconds = 8000, 10
	data := make([]byte, rate*2*seconds)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(rate), uint32(rate * 2), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

func TestB4010_ATenSecondClipIsTranscribedAndBilledAsOneLineAtItsCatalogPrice(t *testing.T) {
	const key = "key-b4010"
	pool := agentBankDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	p.SetLXCSpendSink(store, func() bool { return true })

	fixture, err := os.ReadFile("testdata/deepgram_listen_nova3_10s.json")
	if err != nil {
		t.Fatal(err)
	}
	var heard string
	dg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		heard = r.URL.Path + "?" + r.URL.RawQuery + " " + r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(dg.Close)
	p.SetDeepgram("dg-test-key", dg.URL)

	seamFund(t, pool, b2313WS, b2313Funded)
	agent, err := store.CreateAgent(ctx, b2313WS, "receptionist", "user-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachAgentKey(ctx, b2313WS, agent.ID, key); err != nil {
		t.Fatal(err)
	}
	const fund = int64(10_000_000)
	if _, err := store.FundAgent(ctx, b2313WS, agent.ID, fund); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "question.wav")
	_, _ = fw.Write(tenSecondWAV())
	_ = mw.WriteField("model", "nova-3")
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Talyvor-Workspace", b2313WS)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: key, WorkspaceID: b2313WS}))
	w := httptest.NewRecorder()
	p.HandleTranscription(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got struct{ Text string }
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Text != "What did my agents spend this week?" {
		t.Errorf("text = %q", got.Text)
	}
	if !strings.HasPrefix(heard, "/v1/listen?model=nova-3") || !strings.HasSuffix(heard, " Token dg-test-key") {
		t.Errorf("Deepgram was asked %q", heard)
	}

	m, _ := catalog.Get("nova-3")
	want := settleULXC(10.0 / 60 * m.PerMinute)
	var spends, spent int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(-sum(amount), 0)::bigint
		FROM lxc_ledger WHERE workspace_id = $1 AND type = 'spend'`, b2313WS).Scan(&spends, &spent); err != nil {
		t.Fatal(err)
	}
	if spends != 1 || spent != want {
		t.Errorf("lxc_ledger: %d spend rows totalling %d µLXC — want one line of %d (10 s of nova-3 at $%v a minute)",
			spends, spent, want, m.PerMinute)
	}
	var agentBal, spendAcct int64
	if err := pool.QueryRow(ctx, `SELECT
		COALESCE(sum(amount_ulxc) FILTER (WHERE account = $2), 0)::bigint,
		COALESCE(sum(amount_ulxc) FILTER (WHERE account = 'spend'), 0)::bigint
		FROM agent_postings WHERE workspace_id = $1`, b2313WS, "agent:"+agent.ID).Scan(&agentBal, &spendAcct); err != nil {
		t.Fatal(err)
	}
	if spendAcct != want || agentBal != fund-want {
		t.Errorf("agent_postings: spend %d, agent holds %d — want %d spent, %d left", spendAcct, agentBal, want, fund-want)
	}
}

func TestB4010_WithoutTheDeepgramKeySpeechIsNotConfigured(t *testing.T) {
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	for path, h := range map[string]http.HandlerFunc{"/v1/audio/transcriptions": p.HandleTranscription, "/v1/audio/speech": p.HandleSpeech} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"input":"hello"}`)))
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "speech is not configured") {
			t.Errorf("%s without LENS_DEEPGRAM_API_KEY: %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestB4010_SpeechIsSpokenAndChargedPerCharacter(t *testing.T) {
	pool := agentBankDB(t)
	ctx := context.Background()
	store := economy.NewDualTokenStore(nil, pool, nil)
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	p.SetAgentSpender(store, func() bool { return true })
	p.SetReservation(func() bool { return true }, func() int { return 4096 })
	p.SetLXCSpendSink(store, func() bool { return true })
	var asked string
	dg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3-fake-mp3"))
	}))
	t.Cleanup(dg.Close)
	p.SetDeepgram("dg-test-key", dg.URL)
	seamFund(t, pool, b2313WS, b2313Funded)
	agent, _ := store.CreateAgent(ctx, b2313WS, "voice", "user-owner")
	if err := store.AttachAgentKey(ctx, b2313WS, agent.ID, "key-b4010-tts"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FundAgent(ctx, b2313WS, agent.ID, 10_000_000); err != nil {
		t.Fatal(err)
	}
	const said = "Your agents spent 42 pounds this week."
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"aura-2","input":"`+said+`","voice":"thalia"}`))
	req.Header.Set("X-Talyvor-Workspace", b2313WS)
	req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{APIKeyID: "key-b4010-tts", WorkspaceID: b2313WS}))
	w := httptest.NewRecorder()
	p.HandleSpeech(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "ID3-fake-mp3" || w.Header().Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("status %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	if asked != "/v1/speak?encoding=mp3&model=aura-2-thalia-en" {
		t.Errorf("Deepgram was asked %q", asked)
	}
	m, _ := catalog.Get("aura-2")
	want := settleULXC(float64(len(said)) * m.Per1MChars / 1e6)
	var spends, spent int64
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(-sum(amount), 0)::bigint
		FROM lxc_ledger WHERE workspace_id = $1 AND type = 'spend'`, b2313WS).Scan(&spends, &spent); err != nil {
		t.Fatal(err)
	}
	if spends != 1 || spent != want {
		t.Errorf("lxc_ledger: %d spend rows totalling %d µLXC — want one line of %d", spends, spent, want)
	}
}

func TestB4010_ASpeechModelIsRefusedOnTheChatSeam(t *testing.T) {
	p, _, _ := newLoggingProxy(t, workspace.LoggingFull)
	req := httptest.NewRequest(http.MethodPost, "/v1/proxy/openai/v1/chat/completions",
		strings.NewReader(`{"model":"nova-3","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	p.HandleOpenAI(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "/v1/audio/transcriptions") {
		t.Errorf("a chat request naming nova-3: %d %s — want 400 pointing at /v1/audio", w.Code, w.Body.String())
	}
}
