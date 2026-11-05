package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/talyvor/lens/internal/budgets"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/metrics"
)

// speech.go — B40.10: speech through Lens. OpenAI-compatible POST /v1/audio/transcriptions (audio in, text
// out) and POST /v1/audio/speech (text in, audio out), priced from the catalog — per minute heard, per
// character spoken — and charged through the same holds, rules and charges as a model call: an agent's
// hold is taken before the provider is called and settled to the delivered cost; a person's or a
// workspace's call draws the plan allowance, then prepaid.
//
// Deepgram's EU endpoint first (nova-3, aura-2), OpenAI second (whisper-1, tts-1, tts-1-hd). Without
// LENS_DEEPGRAM_API_KEY a request naming no model is served by OpenAI (B17.81); with neither key both routes
// answer that speech is not configured. Lens keeps no audio:
// the upload is read into memory, sent upstream and dropped; the reply is streamed through.

const (
	defaultTranscriptionModel = "nova-3"
	defaultSpeechModel        = "aura-2"
	maxAudioBytes             = 25 << 20 // OpenAI's own upload limit
	maxSpeechChars            = 4096     // OpenAI's own input limit
	// holdBytesPerSecond bounds a clip's length from its size for the agent's hold before it is heard: 800 bps
	// is far below what speech codecs use (Opus's floor is 6 kbps), so the hold covers the clip and the settle
	// charges what it really was. shortcut: a losslessly compressed silence can still be longer than this bound;
	// the settle then charges the hold and the excess is logged — parse the container's duration if that is seen.
	holdBytesPerSecond = 100
)

// SetDeepgram configures Deepgram. An empty key leaves speech to OpenAI's models, when its key is set.
func (p *Proxy) SetDeepgram(key, baseURL string) {
	p.deepgramKey = key
	p.deepgramURL = strings.TrimRight(baseURL, "/")
}

// openAIAudioURL maps the configured OpenAI chat URL onto its audio endpoint, honouring an override as embeddingsURLFor does.
func (p *Proxy) openAIAudioURL(path string) string {
	u := p.openAIURL
	if i := strings.LastIndex(u, "/chat/completions"); i >= 0 {
		return u[:i] + "/audio/" + path
	}
	return strings.TrimRight(u, "/") + "/v1/audio/" + path
}

// speechModel resolves a requested speech model and the provider that serves it; ok=false answers 400.
func (p *Proxy) speechModel(w http.ResponseWriter, id string, transcribe bool) (catalog.Model, bool) {
	m, ok := catalog.Get(id)
	if !ok || (transcribe && m.PerMinute <= 0) || (!transcribe && m.Per1MChars <= 0) {
		kind := "speech"
		if transcribe {
			kind = "transcription"
		}
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a %s model Lens serves", id, kind))
		return m, false
	}
	if (m.Provider == "openai" && p.openAIKey == "") || (m.Provider == "deepgram" && p.deepgramKey == "") {
		writeError(w, http.StatusServiceUnavailable, "speech is not configured on this Lens for "+id)
		return m, false
	}
	return m, true
}

// speechAdmit runs a model call's pre-serve checks for a speech request costing up to estUSD: the
// workspace's budgets and limits, an agent's hold under its rules, and for anyone else, that allowance plus
// prepaid covers it. It returns the context to settle on, or false once it has answered the refusal.
func (p *Proxy) speechAdmit(w http.ResponseWriter, r *http.Request, wsID string, m catalog.Model, requestID string, estUSD float64) (context.Context, bool) {
	ctx := economy.WithChargeRequest(r.Context(), requestID)
	if p.workspaceManager != nil {
		if policy := p.workspaceManager.CheckPolicy(ctx, wsID, m.Provider, m.ID, 0); !policy.Allowed {
			writeError(w, http.StatusForbidden, policy.Violation)
			metrics.RequestsTotal.WithLabelValues(m.Provider, "workspace_blocked").Inc()
			return ctx, false
		}
	}
	if p.budgetService != nil && p.budgetService.CheckBudget(ctx, wsID, r.Header.Get("X-Talyvor-Team"), r.Header.Get("X-Talyvor-Sprint"), estUSD) == budgets.DecisionBlock {
		writeError(w, http.StatusPaymentRequired, "budget exceeded for workspace/team/sprint")
		metrics.RequestsTotal.WithLabelValues(m.Provider, "budget_blocked").Inc()
		return ctx, false
	}
	if status, msg := p.limits.admit(ctx, wsID, estUSD, 0); status != 0 {
		writeError(w, status, msg)
		metrics.RequestsTotal.WithLabelValues(m.Provider, "workspace_limit_blocked").Inc()
		return ctx, false
	}
	estULXC := economy.ChargeULXC(estUSD)
	if agentKeyID := agentKeyIDFromContext(ctx); agentKeyID != "" {
		ctx = withAgentCall(ctx, agentKeyID, m.ID, m.Provider, "", r.Header.Get("Idempotency-Key"))
		// shortcut: with reservations off (LENS_LXC_RESERVATION_ENABLED=false) an agent's speech is charged to its
		// workspace by the shadow debit, not held on the agent; reservations are on by default.
		if !p.reservationActive() || estULXC <= 0 {
			return ctx, true
		}
		reservationID, err := p.agentDebitKeyFor(ctx, agentKeyID, m.ID, "")
		if err == nil {
			hctx, cancel := context.WithTimeout(ctx, preServeHoldTimeout)
			err = p.agentSpender.ReserveLXCForAgent(hctx, agentKeyID, wsID, reservationID, estULXC,
				economy.AgentDebitMeta{RequestedModel: m.ID, RequestID: requestID})
			cancel()
		}
		if err != nil {
			metrics.RequestsTotal.WithLabelValues(m.Provider, writeAgentRefusal(w, err)).Inc()
			return ctx, false
		}
		return withReservation(ctx, reservationHandle{reservationID: reservationID, requestID: requestID}), true
	}
	_, chat := chatSession(ctx)
	gated := chat || (p.lxcGatingEnabled != nil && p.lxcGatingEnabled() && p.lxcShadowEnabled != nil && p.lxcShadowEnabled())
	if !gated || p.lxcGate == nil || estULXC <= 0 {
		return ctx, true
	}
	var covered int64
	if p.allowance != nil {
		if rem, inPeriod, err := p.allowance.RemainingULXC(ctx, wsID, time.Now()); err == nil && inPeriod {
			covered = rem
		}
	}
	if covered >= estULXC {
		return ctx, true
	}
	balance, err := p.lxcGate.GetUnallocatedLXC(ctx, wsID)
	if err != nil {
		slog.Warn("billing: speech balance read failed (failing open)", slog.String("workspace", wsID), slog.String("err", err.Error()))
		return ctx, true
	}
	if balance < p.withPlatformFee(ctx, wsID, estULXC-covered) {
		writeError(w, http.StatusPaymentRequired, "not enough credit for this request — top up to continue")
		metrics.RequestsTotal.WithLabelValues(m.Provider, "lxc_blocked").Inc()
		return ctx, false
	}
	return ctx, true
}

// speechCharge books a served speech request at costUSD exactly as a served model call is booked: the
// agent's hold settled, else the allowance then prepaid, and the workspace's budgets and limits counted.
func (p *Proxy) speechCharge(ctx context.Context, wsID, model, team, sprint string, costUSD float64) {
	ctx, cancel := detachBounded(ctx) // a client hanging up after the answer still pays for it (B27.4)
	defer cancel()
	if p.reservationActive() {
		p.settleReservationBasis(ctx, costUSD, model, "")
	}
	subscriber := p.chargeChatUsage(ctx, wsID, costUSD) || p.chargeSubscriberUsage(ctx, wsID, costUSD)
	if !p.reservationActive() && !subscriber {
		p.shadowSpendLXC(ctx, wsID, costUSD)
	}
	if p.budgetService != nil {
		p.budgetService.RecordSpend(ctx, wsID, team, sprint, costUSD)
	}
	p.limits.recordSpend(ctx, wsID, costUSD)
}

func (p *Proxy) speechRequest(w http.ResponseWriter, r *http.Request) (wsID, requestID string, ok bool) {
	if p.deepgramKey == "" && p.openAIKey == "" {
		writeError(w, http.StatusServiceUnavailable, "speech is not configured on this Lens")
		return "", "", false
	}
	wsID = defaultWorkspaceID
	if p.workspaceManager != nil {
		wsID = p.workspaceManager.ExtractWorkspaceID(r)
	}
	requestID = r.Header.Get("X-Talyvor-Request-ID")
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Talyvor-Request-ID", requestID)
	return wsID, requestID, true
}

// upstreamRefused answers a provider's non-200: its 4xx is the caller's request (400, with the provider's
// words), anything else is Lens's upstream failing (502). Either way the hold is released and nothing charged.
func (p *Proxy) upstreamRefused(ctx context.Context, w http.ResponseWriter, provider string, status int, body []byte) {
	p.releaseReservation(ctx, "speech_upstream_error")
	metrics.RequestsTotal.WithLabelValues(provider, "upstream_error").Inc()
	if status >= 400 && status < 500 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s refused the request: %s", provider, bytes.TrimSpace(body[:min(len(body), 512)])))
		return
	}
	writeError(w, http.StatusBadGateway, fmt.Sprintf("%s did not answer (HTTP %d); nothing was charged", provider, status))
}

// HandleTranscription serves POST /v1/audio/transcriptions: multipart `file` and `model` (default nova-3, whisper-1 without Deepgram),
// optional `language` and `response_format` (json, the default, or text). Answers {"text": "..."}.
func (p *Proxy) HandleTranscription(w http.ResponseWriter, r *http.Request) {
	wsID, requestID, ok := p.speechRequest(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAudioBytes+1<<20)
	if err := r.ParseMultipartForm(maxAudioBytes + 1<<20); err != nil {
		writeError(w, http.StatusBadRequest, "send the audio as multipart/form-data with a file field, up to 25 MB: "+err.Error())
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	f, fh, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "the audio goes in the multipart field \"file\"")
		return
	}
	audio, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || len(audio) == 0 {
		writeError(w, http.StatusBadRequest, "the audio file is empty or could not be read")
		return
	}
	modelID := r.FormValue("model")
	if modelID == "" {
		modelID = defaultTranscriptionModel
		if p.deepgramKey == "" {
			modelID = "whisper-1"
		}
	}
	format := r.FormValue("response_format")
	if format != "" && format != "json" && format != "text" {
		writeError(w, http.StatusBadRequest, "response_format must be json or text")
		return
	}
	m, ok := p.speechModel(w, modelID, true)
	if !ok {
		return
	}
	boundSeconds := math.Max(1, float64(len(audio))/holdBytesPerSecond)
	ctx, ok := p.speechAdmit(w, r, wsID, m, requestID, boundSeconds/60*m.PerMinute)
	if !ok {
		return
	}

	var text string
	var seconds float64
	if m.Provider == "deepgram" {
		q := url.Values{"model": {m.ID}, "smart_format": {"true"}}
		if lang := r.FormValue("language"); lang != "" {
			q.Set("language", lang)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.deepgramURL+"/v1/listen?"+q.Encode(), bytes.NewReader(audio))
		req.Header.Set("Authorization", "Token "+p.deepgramKey)
		req.Header.Set("Content-Type", fh.Header.Get("Content-Type"))
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/octet-stream")
		}
		var out struct {
			Metadata struct {
				Duration float64 `json:"duration"`
			} `json:"metadata"`
			Results struct {
				Channels []struct {
					Alternatives []struct {
						Transcript string `json:"transcript"`
					} `json:"alternatives"`
				} `json:"channels"`
			} `json:"results"`
		}
		if !p.speechDo(ctx, w, req, m.Provider, &out) {
			return
		}
		seconds = out.Metadata.Duration
		if len(out.Results.Channels) > 0 && len(out.Results.Channels[0].Alternatives) > 0 {
			text = out.Results.Channels[0].Alternatives[0].Transcript
		}
	} else {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		_, _ = mw.CreateFormFile("file", fh.Filename)
		body.Write(audio) // a part writes straight through to body; the concrete Write keeps the audio off any response writer
		_ = mw.WriteField("model", m.ID)
		_ = mw.WriteField("response_format", "verbose_json") // the only shape that says how long the clip was
		if lang := r.FormValue("language"); lang != "" {
			_ = mw.WriteField("language", lang)
		}
		_ = mw.Close()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.openAIAudioURL("transcriptions"), &body)
		req.Header.Set("Authorization", "Bearer "+p.openAIKey)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		var out struct {
			Text     string  `json:"text"`
			Duration float64 `json:"duration"`
		}
		if !p.speechDo(ctx, w, req, m.Provider, &out) {
			return
		}
		text, seconds = out.Text, out.Duration
	}
	if seconds <= 0 { // a provider that did not say how long it heard is charged the bound it was held for
		seconds = boundSeconds
	}
	if seconds > boundSeconds {
		slog.Warn("speech: the clip ran past its hold; an agent is charged the hold", slog.String("request_id", requestID),
			slog.Float64("heard_seconds", seconds), slog.Float64("held_seconds", boundSeconds))
	}
	p.speechCharge(ctx, wsID, m.ID, r.Header.Get("X-Talyvor-Team"), r.Header.Get("X-Talyvor-Sprint"), seconds/60*m.PerMinute)
	metrics.RequestsTotal.WithLabelValues(m.Provider, "success").Inc()
	if format == "text" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, text)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"text": text})
}

// speechDo sends req and decodes a 200's JSON into out; on anything else it has answered the caller.
func (p *Proxy) speechDo(ctx context.Context, w http.ResponseWriter, req *http.Request, provider string, out any) bool {
	resp, err := p.httpClient.Do(req)
	if err != nil {
		p.upstreamRefused(ctx, w, provider, 0, nil)
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || resp.StatusCode != http.StatusOK {
		p.upstreamRefused(ctx, w, provider, resp.StatusCode, body)
		return false
	}
	if err := json.Unmarshal(body, out); err != nil {
		p.upstreamRefused(ctx, w, provider, http.StatusBadGateway, nil)
		return false
	}
	return true
}

// deepgramEncodings maps OpenAI's response_format onto Deepgram's speak parameters.
var deepgramEncodings = map[string]url.Values{
	"mp3":  {"encoding": {"mp3"}},
	"opus": {"encoding": {"opus"}},
	"aac":  {"encoding": {"aac"}},
	"flac": {"encoding": {"flac"}},
	"wav":  {"encoding": {"linear16"}, "container": {"wav"}},
}

// HandleSpeech serves POST /v1/audio/speech: JSON {"model" (default aura-2, tts-1 without Deepgram), "input", "voice",
// "response_format" (mp3, the default, opus, aac, flac or wav)}. Answers the audio.
func (p *Proxy) HandleSpeech(w http.ResponseWriter, r *http.Request) {
	wsID, requestID, ok := p.speechRequest(w, r)
	if !ok {
		return
	}
	var in struct {
		Model          string `json:"model"`
		Input          string `json:"input"`
		Voice          string `json:"voice"`
		ResponseFormat string `json:"response_format"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	chars := utf8.RuneCountInString(in.Input)
	if chars == 0 || chars > maxSpeechChars {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("input must be 1 to %d characters", maxSpeechChars))
		return
	}
	if in.Model == "" {
		in.Model = defaultSpeechModel
		if p.deepgramKey == "" {
			in.Model = "tts-1"
		}
	}
	if in.ResponseFormat == "" {
		in.ResponseFormat = "mp3"
	}
	enc, known := deepgramEncodings[in.ResponseFormat]
	if !known {
		writeError(w, http.StatusBadRequest, "response_format must be mp3, opus, aac, flac or wav")
		return
	}
	m, ok := p.speechModel(w, in.Model, false)
	if !ok {
		return
	}
	costUSD := float64(chars) * m.Per1MChars / 1e6 // known before the call: the hold is the charge
	ctx, ok := p.speechAdmit(w, r, wsID, m, requestID, costUSD)
	if !ok {
		return
	}

	var req *http.Request
	if m.Provider == "deepgram" {
		voice := strings.ToLower(in.Voice)
		if voice == "" {
			voice = "thalia"
		}
		if !strings.Contains(voice, "-") {
			voice += "-en"
		}
		q := url.Values{"model": {m.ID + "-" + voice}}
		for k, v := range enc {
			q[k] = v
		}
		body, _ := json.Marshal(map[string]string{"text": in.Input})
		req, _ = http.NewRequestWithContext(ctx, http.MethodPost, p.deepgramURL+"/v1/speak?"+q.Encode(), bytes.NewReader(body))
		req.Header.Set("Authorization", "Token "+p.deepgramKey)
	} else {
		if in.Voice == "" {
			in.Voice = "alloy"
		}
		body, _ := json.Marshal(map[string]string{"model": m.ID, "input": in.Input, "voice": in.Voice, "response_format": in.ResponseFormat})
		req, _ = http.NewRequestWithContext(ctx, http.MethodPost, p.openAIAudioURL("speech"), bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+p.openAIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		p.upstreamRefused(ctx, w, m.Provider, 0, nil)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		p.upstreamRefused(ctx, w, m.Provider, resp.StatusCode, body)
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	_, _ = io.Copy(w, resp.Body)
	p.speechCharge(ctx, wsID, m.ID, r.Header.Get("X-Talyvor-Team"), r.Header.Get("X-Talyvor-Sprint"), costUSD)
	metrics.RequestsTotal.WithLabelValues(m.Provider, "success").Inc()
}
