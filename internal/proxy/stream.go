package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/alerts"
	"github.com/talyvor/lens/internal/attribution"
	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/inference"
	"github.com/talyvor/lens/internal/keypool"
	"github.com/talyvor/lens/internal/metrics"
	"github.com/talyvor/lens/internal/retry"
	"github.com/talyvor/lens/internal/router"
	"github.com/talyvor/lens/internal/workspace"
	"github.com/talyvor/lens/internal/worktier"
)

// streamUsage accumulates the provider's reported token counts as they
// arrive across the stream (OpenAI's final usage chunk; Anthropic's
// message_start input + message_delta output). present=false means the
// stream surfaced no usage, so the caller bills via the len/4 estimate.
type streamUsage struct {
	inputTokens  int
	outputTokens int
	// Cache-aware breakdown (mirrors inference.Usage), populated by extractUsage so the streamed settle
	// prices cache reads/writes at their own rate like the buffered path. Zero when the provider reports
	// no caching → uncachedInputTokens == inputTokens and the basis is flat, identical to before.
	uncachedInputTokens   int
	cachedInputTokens     int
	cacheWriteInputTokens int
	present               bool
}

// streamSpend carries the per-request billing context into the stream
// handler so a completed stream can record spend — the piece that used to be
// missing, leaving streamed requests invisible to budgets/alerts.
type streamSpend struct {
	wsID, team, sprint, feature string
	model                       string
	asked                       string // B27.32: the model the request asked for; "" = the model sent
	requestID, sessionID        string
	modality                    string
	logging                     workspace.LoggingPolicy
	estInputTokens              int              // fallback input estimate when no usage is emitted
	tare                        alerts.TareMeter // B6.4: the Tare record for this request; zero = not reduced
	distillMethod               string           // B7.3: "convert" when this request was distilled, as on the buffered row
	visionOCR                   visionSpend      // B7.3: the OCR sub-call's cost, owed its own vision_ocr row
	reqFP                       string           // B15.1: the request fingerprint the cache write is keyed under
	turn                        cache.Turn       // B16.1: the latest question + history hash the semantic write answers
	post                        streamPostServe  // B15.3: what the buffered seam's post-flush records read
	byok                        bool             // B27.26: sent on the workspace's own key, so charged nothing
}

// streamPostServe carries what the buffered path's post-flush records read and a stream would
// otherwise not have (B15.3): the prompt as sent, the routing evidence, the request's start. Zero
// value = nothing to record beyond the spend row, which is what a direct StreamHandler call gets.
type streamPostServe struct {
	provider              string
	requestBody           []byte
	requestStart          time.Time
	trackSession          bool // X-Talyvor-Session named a tracked session
	piiDetected           bool
	guardrailFired        bool
	compressedPrompt      string // the prompt sent upstream; equals the caller's prompt unless compressed
	savingsPct            float64
	compressionGateOpened bool
	routeWasAuto          bool
	baselineModel         string
	cohortBasis           string
	cohortOverrode        bool
	cohortN               int
	cohortModel           string
	complexityBucket      string
	distillFacts          []distillServeFact
}

const (
	openAIDoneMarker = "[DONE]"
	// reportChargeHeader asks for chargeFrame: what the answer was charged, in µLXC, before the stream's
	// terminator (B17.129; Chat's chatStream.ts CHARGE_FRAME reads it).
	reportChargeHeader = "X-Talyvor-Report-Charge"
	chargeFrame        = "talyvor.charge"
	sseScannerMax      = 1 << 20 // 1 MiB per SSE line, plenty for any chunk
)

// chargeTallyKey carries what each chat request served for one client request was charged, in µLXC, in order
// (B17.265): Run code and web search serve several, and a stream asked to report its charge gets a frame for each.
type chargeTallyKey struct{}

// withChargeTally is r carrying a tally, the one it carries already if it has one.
func withChargeTally(r *http.Request) (*http.Request, *[]int64) {
	if t, ok := r.Context().Value(chargeTallyKey{}).(*[]int64); ok {
		return r, t
	}
	t := &[]int64{}
	return r.WithContext(context.WithValue(r.Context(), chargeTallyKey{}, t)), t
}

func tallyCharge(ctx context.Context, ulxc int64) {
	if t, ok := ctx.Value(chargeTallyKey{}).(*[]int64); ok {
		*t = append(*t, ulxc)
	}
}

// writeChargeFrames sends a stream asked to report its charge one chargeFrame per charge.
func writeChargeFrames(w io.Writer, r *http.Request, charges []int64) {
	if r.Header.Get(reportChargeHeader) != "true" {
		return
	}
	for _, c := range charges {
		_, _ = fmt.Fprintf(w, "event: %s\ndata: {\"type\":%q,\"charged_ulxc\":%d}\n\n", chargeFrame, chargeFrame, c) // nosemgrep: go.lang.security.audit.xss.no-fprintf-to-responsewriter.no-fprintf-to-responsewriter -- text/event-stream frame of a constant and a number
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// StreamHandler proxies streaming LLM responses back to the client SSE-style
// while accumulating the assembled text content for caching + learning.
type StreamHandler struct {
	proxy *Proxy
}

// streamOps abstracts the per-provider differences in stream parsing and
// cache-payload synthesis. The transport mechanics (fetch upstream, scan
// SSE lines, flush, accumulate, store) are identical and live in serve().
type streamOps interface {
	upstreamURL() string
	applyAuth(*http.Request)
	// prepareBody adjusts the upstream request body before streaming — e.g.
	// injecting stream_options.include_usage for OpenAI-family providers so
	// the final chunk carries a usage block. Identity for providers that
	// emit usage natively (Anthropic).
	prepareBody(body []byte) []byte
	// processLine inspects an SSE line, appends any extracted content to
	// the accumulator, and reports whether the stream should terminate.
	processLine(line []byte, accumulated *strings.Builder) (done bool)
	// endsAnswer reports whether line is the provider's end-of-answer event (OpenAI's [DONE],
	// Anthropic's message_stop). A stream whose connection closes before that line was cut off,
	// and its text is never cached (B15.2).
	endsAnswer(line []byte) bool
	// applyPoolKey replaces the deployment key applyAuth set with a pooled one (B15.3).
	applyPoolKey(req *http.Request, key string)
	// extractUsage inspects one SSE line for provider-reported usage,
	// updating u in place. No-op for lines that carry no usage.
	extractUsage(line []byte, u *streamUsage)
	// synthesizeCachePayload produces the JSON to cache once the stream
	// has fully been consumed.
	synthesizeCachePayload(accumulated string) []byte
}

type openAIStreamOps struct {
	url     string
	setAuth func(*http.Request)
}

func (o openAIStreamOps) upstreamURL() string         { return o.url }
func (o openAIStreamOps) applyAuth(req *http.Request) { o.setAuth(req) }
func (openAIStreamOps) applyPoolKey(req *http.Request, key string) {
	req.Header.Set("Authorization", "Bearer "+key)
}

// prepareBody injects stream_options.include_usage so OpenAI-compatible
// providers emit a final usage chunk. Best-effort: a parse failure returns
// the body untouched — metering must never break the stream.
func (openAIStreamOps) prepareBody(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	so, _ := m["stream_options"].(map[string]any)
	if so == nil {
		so = map[string]any{}
	}
	so["include_usage"] = true
	m["stream_options"] = so
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// extractUsage reads the final chunk's usage block (present once
// include_usage is set). Intermediate chunks carry usage:null and are
// skipped via the pointer.
func (openAIStreamOps) extractUsage(line []byte, u *streamUsage) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if bytes.Equal(payload, []byte(openAIDoneMarker)) {
		return
	}
	var chunk struct {
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &chunk) != nil || chunk.Usage == nil {
		return
	}
	cached, write := 0, 0
	if d := chunk.Usage.PromptTokensDetails; d != nil {
		cached, write = d.CachedTokens, d.CacheWriteTokens
	}
	uncached := chunk.Usage.PromptTokens - cached - write // cached/write are a subset of prompt_tokens
	if uncached < 0 {
		uncached = 0
	}
	u.inputTokens = chunk.Usage.PromptTokens // legacy total (incl cached) — unchanged
	u.outputTokens = chunk.Usage.CompletionTokens
	u.uncachedInputTokens = uncached
	u.cachedInputTokens = cached
	u.cacheWriteInputTokens = write
	u.present = true
}

func (openAIStreamOps) endsAnswer(line []byte) bool {
	return bytes.Equal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), []byte(openAIDoneMarker))
}

func (openAIStreamOps) processLine(line []byte, acc *strings.Builder) bool {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if bytes.Equal(payload, []byte(openAIDoneMarker)) {
		return true
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return false
	}
	for _, c := range chunk.Choices {
		acc.WriteString(c.Delta.Content)
	}
	return false
}

func (openAIStreamOps) synthesizeCachePayload(accumulated string) []byte {
	out, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"role": "assistant", "content": accumulated}},
		},
	})
	return out
}

type anthropicStreamOps struct {
	url     string
	setAuth func(*http.Request)
}

func (a anthropicStreamOps) upstreamURL() string         { return a.url }
func (a anthropicStreamOps) applyAuth(req *http.Request) { a.setAuth(req) }
func (anthropicStreamOps) applyPoolKey(req *http.Request, key string) {
	req.Header.Set("x-api-key", key)
}

// prepareBody is identity: Anthropic emits usage natively (message_start +
// message_delta), so there is no include_usage flag to inject.
func (anthropicStreamOps) prepareBody(body []byte) []byte { return body }

// extractUsage reuses the canonical Anthropic chunk normaliser
// (internal/cache) for OUTPUT tokens rather than re-parsing message_delta
// usage here, and parses the message_start event — which that normaliser
// (content + output focused) doesn't surface — for INPUT tokens.
func (anthropicStreamOps) extractUsage(line []byte, u *streamUsage) {
	if chunk, ok := cache.NormalizeAnthropicChunk(string(line)); ok && chunk.IsFinal && chunk.TokenDelta > 0 {
		u.outputTokens = chunk.TokenDelta
		u.present = true
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(payload, &ev) == nil && ev.Type == "message_start" && ev.Message.Usage.InputTokens > 0 {
		un := ev.Message.Usage
		u.inputTokens = un.InputTokens // legacy: uncached only (cache read/write are disjoint) — unchanged
		u.uncachedInputTokens = un.InputTokens
		u.cachedInputTokens = un.CacheReadInputTokens
		u.cacheWriteInputTokens = un.CacheCreationInputTokens
		u.present = true
	}
}

func (anthropicStreamOps) endsAnswer(line []byte) bool {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	var ev struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &ev) == nil && ev.Type == "message_stop"
}

// endsClientStream reports whether line, as the client is sent it, opens the stream's terminator:
// `data: [DONE]`, or Anthropic's `event: message_stop` or its data line.
func endsClientStream(line []byte) bool {
	return openAIStreamOps{}.endsAnswer(line) || anthropicStreamOps{}.endsAnswer(line) ||
		bytes.Equal(bytes.TrimSpace(line), []byte("event: message_stop"))
}

func (anthropicStreamOps) processLine(line []byte, acc *strings.Builder) bool {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	var chunk struct {
		Type  string `json:"type"`
		Delta struct {
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return false
	}
	if chunk.Type == "content_block_delta" {
		acc.WriteString(chunk.Delta.Text)
	}
	// Anthropic doesn't have a single "DONE" marker like OpenAI; the
	// connection close ends the stream. We could return true on
	// message_stop to break early, but EOF works fine too — keep it simple.
	return false
}

func (anthropicStreamOps) synthesizeCachePayload(accumulated string) []byte {
	out, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": accumulated},
		},
	})
	return out
}

func (s *StreamHandler) serve(
	w http.ResponseWriter,
	r *http.Request,
	provider string,
	model string,
	prompt string,
	cachePrompt string,
	body []byte,
	piiDetected bool,
	sc streamSpend,
	ops streamOps,
) error {
	// Headers must be committed BEFORE the first write/flush.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	// Ask the provider to surface usage in the stream (OpenAI-family:
	// stream_options.include_usage; identity for Anthropic). Best-effort.
	body = ops.prepareBody(body)
	// GPT-6 rejects max_tokens and temperature — the same rewrite forward applies (inference.AdaptReasoningParams),
	// for the model the body is SENT to, which routing may have changed (sc.model).
	upstreamModel := sc.model
	if upstreamModel == "" {
		upstreamModel = model
	}
	body = inference.AdaptReasoningParams(upstreamModel, body)

	// The key pool, as forwardWithFallback uses it: a healthy pooled key for this provider when the
	// operator configured any, else the deployment's single key. A transport failure counts against it.
	var poolKey *keypool.PoolKey
	if s.proxy.keyPool != nil && !sc.byok { // B27.26: a BYOK stream's ops already carry its own key
		if pk, perr := s.proxy.keyPool.Get(provider); perr == nil && pk != nil {
			poolKey = pk
		}
	}

	// Retry the initial upstream call on transient failures. Once we
	// commit to streaming (after WriteHeader below) there's no second
	// chance, so retries only cover the connection-establishment phase.
	// Measure the upstream connection/initial-response latency (the
	// retry.Do block), NOT the whole stream forward. Record on every outcome
	// with a bounded provider+status label; never alters control flow/errors.
	upstreamStart := time.Now()
	result := retry.Do(r.Context(), retry.DefaultConfig(), func(c context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(c, http.MethodPost, ops.upstreamURL(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		// ⚠ THE SECOND HEADER-COPY SEAM. This loop is streaming's own copy of forward's — it does not
		// go through RunUpstream — so the caller's Talyvor credential has to be removed HERE TOO. A
		// fix applied only at forward would have been inert on every streamed request, and streaming
		// is not a corner of the serving path. StripCredentialHeaders is the single stripper both
		// seams share; upstream_credential_leak_test.go drives this one through the real handler.
		for name, values := range auth.StripCredentialHeaders(r.Header) {
			// Skip Host, and Accept-Encoding: forwarding the client's Accept-Encoding disables Go's
			// transparent gzip decoding, so a gzipped upstream stream would be relayed as compressed
			// bytes under text/event-stream — garbage to a streaming client. Let the transport decode.
			if strings.EqualFold(name, "Host") || strings.EqualFold(name, "Accept-Encoding") {
				continue
			}
			for _, v := range values {
				req.Header.Add(name, v)
			}
		}
		ops.applyAuth(req)
		if poolKey != nil {
			ops.applyPoolKey(req, poolKey.Key)
		}
		return s.proxy.httpClient.Do(req)
	})
	if poolKey != nil {
		if result.LastError != nil {
			s.proxy.keyPool.RecordError(poolKey.ID)
		} else {
			s.proxy.keyPool.RecordSuccess(poolKey.ID)
		}
	}
	if result.LastError != nil {
		metrics.RecordUpstream(upstreamProviderLabel(provider), "error", time.Since(upstreamStart))
		writeError(w, http.StatusBadGateway, "upstream LLM error: "+result.LastError.Error())
		s.proxy.releaseReservation(r.Context(), "upstream error") // B17.128: nothing was served
		return result.LastError
	}
	resp := result.Response
	defer resp.Body.Close()
	metrics.RecordUpstream(upstreamProviderLabel(provider), upstreamStatusClass(resp, nil), time.Since(upstreamStart))

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(errBody) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- the upstream error is sent as text/event-stream with nosniff, never as HTML
		s.proxy.releaseReservation(r.Context(), fmt.Sprintf("upstream status %d", resp.StatusCode))
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	if result.Attempts > 1 {
		w.Header().Set("X-Talyvor-Attempts", strconv.Itoa(result.Attempts))
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var accumulated strings.Builder
	var usage streamUsage
	ended := false // the provider sent its end-of-answer event
	// B18.7: the upstream's lines (Bedrock's are decoded from its binary event stream), and what the
	// client is sent for each (Google and Bedrock are translated to OpenAI chunks).
	src := upstreamLines(ops, resp.Body)
	translator, translates := ops.(clientTranslator)
	write := func(lines [][]byte) {
		for _, l := range lines {
			_, _ = w.Write(l) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- text/event-stream lines from the model, sent with nosniff
			_, _ = w.Write([]byte("\n"))
		}
		if flusher != nil && len(lines) > 0 {
			flusher.Flush()
		}
	}
	// B17.129: asked to report the charge, the terminator and anything after it are held back, so the
	// charge frame, written once the answer is booked, comes before it — Chat's reader stops at the terminator.
	reportCharge := r.Header.Get(reportChargeHeader) == "true"
	var held [][]byte
	send := func(lines [][]byte) {
		for i, l := range lines {
			if reportCharge && (held != nil || endsClientStream(l)) {
				held = append(held, lines[i:]...)
				lines = lines[:i]
				break
			}
		}
		write(lines)
	}

	for src.Next() {
		// Stop forwarding if the client disconnected.
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		default:
		}

		line := src.Line()
		// Reconstruct the SSE wire format on the way out: each scanner line
		// strips its trailing \n; the original blank-line separators arrive
		// as zero-length lines. Writing line+"\n" round-trips both.
		if translates {
			send(translator.toClient(line))
		} else {
			send([][]byte{line})
		}

		ops.extractUsage(line, &usage)
		if ops.endsAnswer(line) {
			ended = true
		}
		if ops.processLine(line, &accumulated) {
			break
		}
	}
	if err := src.Err(); err != nil {
		return err
	}
	if translates {
		send(translator.clientTail(usage))
	}

	// Post-serve seam (cache write, learner, streamed spend + reservation SETTLE). Detach from the
	// request's cancellation so a client that disconnects after the stream completes can't abort the
	// bill — we already paid the provider. WithoutCancel (NOT Background) keeps r's VALUES, including
	// the reservation handle threaded onto r in serve(), so the streamed settle actually FIRES in-band
	// instead of stranding the hold for the sweeper to refund (which made streaming requests free).
	storeCtx := withCacheStoreHeader(context.WithoutCancel(r.Context()), r)
	// B17.282: the bill first, as on the buffered seam — the guardrail, the scorer and the cache writes below can
	// take seconds, and a client reading its ledger meanwhile saw the hold and no spend row.
	// Close the streamed-spend gap: bill on the captured provider usage when
	// present, else the len/4 estimate. A streamed request must never again
	// be invisible to budgets/alerts.
	spendCtx, cancelSpend := detachBounded(storeCtx)
	settled, chatULXC := s.proxy.recordStreamSpend(spendCtx, sc, usage, accumulated.String())
	cancelSpend()
	if reportCharge {
		if chatULXC >= 0 {
			write([][]byte{[]byte("event: " + chargeFrame), []byte(fmt.Sprintf(`data: {"type":%q,"charged_ulxc":%d}`, chargeFrame, chatULXC)), {}})
		}
		write(held)
	}
	cached := ops.synthesizeCachePayload(accumulated.String())
	// B15.2: a streamed answer is stored only if it ENDED (a stream cut off before the provider's
	// end event is a partial answer; a cancelled one already returned above) and, where the
	// workspace's output guardrails are on, only if the finished text passes them. They cannot run
	// while a stream is in flight, so this client already has the text — but an answer that fails
	// them must never be served to the next asker, in this workspace or through the pool.
	shouldCache := !piiDetected && ended
	if shouldCache && s.proxy.guardrails != nil && s.proxy.guardrails.OutputEnabled() {
		if ogr := s.proxy.guardrails.CheckOutput(storeCtx, sc.wsID, accumulated.String()); !ogr.Passed || len(ogr.Violations) > 0 {
			shouldCache = false
		}
	}
	// Score every finished answer, as the buffered path scores every 200: the score gates the cache
	// here and feeds the routing corpus below (B15.3). Against the accumulated text content, not the
	// synthesized JSON, so the heuristics see what the user would see. A cut-off answer is not scored.
	var qualityScore float64
	scored := false
	if ended && s.proxy.scorer != nil {
		q := s.proxy.scorer.ScoreResponse(storeCtx, prompt, accumulated.String(), provider, model)
		qualityScore, scored = q.Score, true
		if !q.ShouldCache {
			shouldCache = false
		}
	}
	if shouldCache {
		// Use the workspace-scoped prompt for the cache key so streamed
		// responses respect tenant isolation just like buffered ones. The raw
		// prompt + wsID also feed the opt-in pooled (cross-tenant) write.
		wctx, cancel := detachBounded(storeCtx) // B39.4: storeCtx has no deadline; each write after the answer gets its own
		s.proxy.storeAnswer(wctx, provider, model, cachePrompt, prompt, sc.reqFP, sc.turn, sc.wsID, sc.requestID, cached)
		cancel()
	}
	// W4.9 SHADOW POOL LOG — the streaming lane carries the SAME paid provider call as the buffered
	// one, so leaving it out would make the measured pooled hit rate a statement about half the
	// traffic while reading as a statement about all of it. Gated on the same shouldCache as the
	// cache write above (an uncacheable response could never have been pooled) and on the logging
	// policy, exactly as the buffered seam is. Void, post-serve, default-off.
	if shouldCache && s.proxy.loggingPolicyFor(sc.wsID) != workspace.LoggingNone {
		s.proxy.shadowPoolObservation(storeCtx, sc.wsID, provider, model, prompt, sc.reqFP,
			s.proxy.sharesAnswers(storeCtx, sc.wsID))
	}
	eventPrompt := prompt
	if piiDetected && s.proxy.piiDetector != nil {
		eventPrompt = s.proxy.piiDetector.Detect(prompt).Redacted
	}
	// Gated on the logging policy inside recordTokenEvent (full only) — sc.wsID
	// is the same workspace recordStreamSpend above reads its policy from, so a
	// none/metadata streamed request feeds neither the learner nor the spend row.
	s.proxy.recordTokenEvent(storeCtx, provider, model, eventPrompt, cached, sc.post.savingsPct, piiDetected, sc.wsID)
	postCtx, cancelPost := detachBounded(storeCtx)
	defer cancelPost()
	s.proxy.recordStreamPostServe(postCtx, r, sc, usage, prompt, accumulated.String(), cached, qualityScore, scored, settled)
	return nil
}

// recordStreamPostServe is the streamed seam's copy of the buffered path's post-flush records (B15.3),
// under the same gates in the same order: the session turn, the routing corpus (earn or capture), the
// work tier, the route decision and its prediction, the compression measurement, the distill serves and
// the per-request attribution. Void and post-serve like every one of them — nothing here can reach the
// client, who already has the answer. Two buffered records are NOT here, because a stream has committed
// its headers before the answer exists: the session totals and quality score headers, and the K4 output
// verdict, whose id the caller only ever receives in a header.
func (p *Proxy) recordStreamPostServe(ctx context.Context, r *http.Request, sc streamSpend, u streamUsage,
	prompt, outputText string, payload []byte, quality float64, scored bool, settledChargeUSD float64) {
	pp := sc.post
	inT, outT, estimated, servedCostUSD := streamServedCost(sc, u, outputText)
	if pp.trackSession && sc.logging != workspace.LoggingNone {
		p.recordSessionTurn(ctx, sc.sessionID, prompt, outputText, sc.model, inT, outT, servedCostUSD, false)
	}
	if p.alertManager != nil && sc.logging != workspace.LoggingNone {
		sentTokens := len(pp.compressedPrompt) / 4
		latencyMs := time.Since(pp.requestStart).Milliseconds()
		bucket := string(worktier.ComplexityBucketFor(router.AnalyseComplexity(pp.compressedPrompt).Score()))
		if !p.earnPattern(ctx, pp.piiDetected, pp.guardrailFired, sc.logging, sc.feature, sc.model, pp.provider, prompt, payload,
			sentTokens, outT, quality, scored, latencyMs, bucket) {
			p.capturePattern(ctx, pp.piiDetected, pp.guardrailFired, sc.logging, sc.wsID, sc.feature, sc.model, pp.provider,
				sentTokens, outT, quality, scored, latencyMs, false, bucket)
		}
		p.captureWorkTier(ctx, sc.wsID, sc.feature, sc.model, pp.provider, pp.compressedPrompt,
			sentTokens, outT, pp.piiDetected, pp.guardrailFired, string(sc.logging))
		if pp.routeWasAuto {
			tk := routeTokens{UncachedInput: sentTokens, Output: outT}
			if u.present {
				tk = routeTokens{UncachedInput: u.uncachedInputTokens, CachedInput: u.cachedInputTokens,
					CacheWriteInput: u.cacheWriteInputTokens, Output: u.outputTokens, ProviderReported: true}
			}
			p.captureRouteDecision(ctx, sc.wsID, pp.baselineModel, sc.model, pp.cohortBasis, pp.cohortOverrode, pp.cohortN, tk)
			p.emitRoutingPrediction(ctx, sc.wsID, sc.feature, pp.cohortModel, pp.provider, pp.cohortOverrode, sentTokens, pp.complexityBucket)
		}
		if pp.compressionGateOpened {
			p.captureCompression(ctx, compressionObservation{
				requestID: sc.requestID, workspaceID: sc.wsID, model: sc.model,
				original: prompt, sent: pp.compressedPrompt,
				billedInputTokens: inT, costEstimated: estimated,
			})
		}
		p.recordDistillServes(ctx, sc.wsID, sc.logging, pp.distillFacts, settledChargeUSD)
	}
	if p.attrStore != nil && sc.logging != workspace.LoggingNone {
		attrCtx := attribution.ExtractFromRequest(r)
		attrCtx.RequestID = sc.requestID
		p.attributeRequester(r, sc.wsID, &attrCtx)
		p.attrStore.RecordAsync(attrCtx, inT, outT, servedCostUSD, sc.model, pp.provider, time.Since(pp.requestStart))
	}
}
