package cache

// stream.go — production streaming-response cache. Captures the
// chunks of a live SSE stream as they flow back to the client,
// stores them with their per-chunk timing, and replays them
// later in one of three speeds (instant / fast / realistic).
//
// Provider-specific SSE formats are normalised on capture into a
// neutral StreamChunk and re-serialised on replay so a cached
// Anthropic stream can also serve OpenAI clients (and vice versa).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ─── constants ───────────────────────────────────

const (
	// MaxStreamEntryBytes is the size ceiling for one cached
	// stream. 512KB matches the spec — beyond that we'd be
	// caching whole novels of generated text, which is rarely
	// worth the Redis memory.
	MaxStreamEntryBytes = 512 * 1024

	// FastReplayDelayCap is the per-chunk wait under
	// ReplayFast. Caller gets a noticeably-streaming UX without
	// the original "model thinking" pauses.
	FastReplayDelayCap = 50 * time.Millisecond

	// streamKeyPrefix segregates streaming cache entries from
	// the existing non-streaming exact cache so collisions can't
	// happen across replay strategies.
	streamKeyPrefix = "stream:"
)

// ─── types ───────────────────────────────────────

// StreamChunk is the provider-neutral representation of one
// chunk of generated text. Captures the timing so replay can
// reproduce the original feel.
type StreamChunk struct {
	Text       string `json:"text"`
	TokenDelta int    `json:"token_delta"`
	DelayMs    int    `json:"delay_ms"`
	IsFinal    bool   `json:"is_final"`
}

// StreamEntry is the persisted shape — what GetStream returns
// and SetStream accepts.
type StreamEntry struct {
	Chunks      []StreamChunk `json:"chunks"`
	Model       string        `json:"model"`
	Provider    string        `json:"provider"`
	TotalTokens int           `json:"total_tokens"`
	CreatedAt   time.Time     `json:"created_at"`
	TTL         time.Duration `json:"ttl_seconds"`
}

// ReplayStrategy selects the wait-between-chunks policy.
type ReplayStrategy string

const (
	ReplayInstant   ReplayStrategy = "instant"
	ReplayRealistic ReplayStrategy = "realistic"
	ReplayFast      ReplayStrategy = "fast"
)

// DefaultReplayStrategy is what main.go installs when nothing
// else is specified. Fast strikes the right balance — still
// feels live but doesn't burn time on the original pauses.
const DefaultReplayStrategy = ReplayFast

// ─── capture ─────────────────────────────────────

// teeReader is the io.ReadCloser CaptureStream returns. It
// passes bytes through to the client while a second goroutine
// scans them and reconstructs the chunk timeline. We never
// block the client — capture happens off the hot path.
type teeReader struct {
	upstream   io.ReadCloser
	pipeReader *io.PipeReader
	pipeWriter *io.PipeWriter
	provider   string
}

// CaptureStream returns a wrapped reader that:
//  1. Passes every byte through unchanged.
//  2. Forks a goroutine that scans the same bytes for SSE
//     events, normalises them, and emits a StreamChunk per
//     chunk + a final StreamEntry on EOF.
//
// onChunk and onComplete may be nil if the caller only wants
// passthrough (e.g. on a partial cache miss where capture would
// be wasteful).
func CaptureStream(
	upstream io.ReadCloser,
	provider string,
	onChunk func(chunk StreamChunk),
	onComplete func(entry StreamEntry),
) io.ReadCloser {
	pr, pw := io.Pipe()
	tr := &teeReader{
		upstream:   upstream,
		pipeReader: pr,
		pipeWriter: pw,
		provider:   provider,
	}
	go tr.capture(provider, onChunk, onComplete)
	return tr
}

// Read passes bytes straight through; the side-effect channel
// (pipe) feeds the capture goroutine in parallel.
func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.upstream.Read(p)
	if n > 0 {
		// Best-effort fan-out — if the capture pipe is slow
		// (it shouldn't be) we drop the duplicate rather than
		// stall the client.
		_, _ = t.pipeWriter.Write(p[:n])
	}
	if err != nil {
		// Closing the writer signals EOF to the capture
		// goroutine.
		_ = t.pipeWriter.Close()
	}
	return n, err
}

// Close closes both the upstream and the capture pipe.
func (t *teeReader) Close() error {
	_ = t.pipeWriter.Close()
	return t.upstream.Close()
}

// capture is the goroutine that scans the duplicated byte
// stream, normalises chunks, and assembles the final entry.
func (t *teeReader) capture(
	provider string,
	onChunk func(StreamChunk),
	onComplete func(StreamEntry),
) {
	defer t.pipeReader.Close()
	scanner := bufio.NewScanner(t.pipeReader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	// SSE events are separated by blank lines. We split on
	// double-newline so each "split" is one complete event.
	scanner.Split(splitSSEEvents)

	var (
		entry StreamEntry
		last  time.Time
	)
	entry.Provider = provider
	entry.CreatedAt = time.Now()
	last = entry.CreatedAt

	for scanner.Scan() {
		raw := scanner.Text()
		var chunk StreamChunk
		var ok bool
		switch provider {
		case "anthropic":
			chunk, ok = NormalizeAnthropicChunk(raw)
		case "openai":
			chunk, ok = NormalizeOpenAIChunk(raw)
		default:
			// Generic fallback — treat each event as one chunk
			// of opaque text.
			chunk, ok = NormalizeGenericChunk(raw)
		}
		if !ok {
			continue
		}
		now := time.Now()
		chunk.DelayMs = int(now.Sub(last).Milliseconds())
		last = now
		entry.Chunks = append(entry.Chunks, chunk)
		entry.TotalTokens += chunk.TokenDelta
		if onChunk != nil {
			onChunk(chunk)
		}
		if chunk.IsFinal {
			break
		}
	}
	if onComplete != nil {
		onComplete(entry)
	}
}

// splitSSEEvents is the bufio.Scanner SplitFunc that yields one
// SSE event per call. Events are terminated by "\n\n"; we trim
// trailing newlines on output.
func splitSSEEvents(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2, bytes.TrimRight(data[:i], "\r\n"), nil
	}
	if atEOF {
		return len(data), bytes.TrimRight(data, "\r\n"), nil
	}
	return 0, nil, nil
}

// ─── normalisation: parse provider SSE → StreamChunk ────

// NormalizeAnthropicChunk parses one Anthropic SSE event.
// Returns (chunk, true) on success, (zero, false) when the
// event isn't a content chunk we care about (ping events,
// message_start metadata, etc.).
func NormalizeAnthropicChunk(raw string) (StreamChunk, bool) {
	dataLine := extractSSEData(raw)
	if dataLine == "" {
		return StreamChunk{}, false
	}
	if dataLine == "[DONE]" {
		return StreamChunk{IsFinal: true}, true
	}
	var payload struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(dataLine), &payload); err != nil {
		return StreamChunk{}, false
	}
	switch payload.Type {
	case "content_block_delta":
		return StreamChunk{
			Text:       payload.Delta.Text,
			TokenDelta: estimateTokens(payload.Delta.Text),
		}, true
	case "message_stop", "message_delta":
		return StreamChunk{IsFinal: true, TokenDelta: payload.Usage.OutputTokens}, true
	}
	return StreamChunk{}, false
}

// NormalizeOpenAIChunk parses one OpenAI SSE event.
func NormalizeOpenAIChunk(raw string) (StreamChunk, bool) {
	dataLine := extractSSEData(raw)
	if dataLine == "" {
		return StreamChunk{}, false
	}
	if dataLine == "[DONE]" {
		return StreamChunk{IsFinal: true}, true
	}
	var payload struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(dataLine), &payload); err != nil {
		return StreamChunk{}, false
	}
	if len(payload.Choices) == 0 {
		return StreamChunk{}, false
	}
	c := payload.Choices[0]
	chunk := StreamChunk{
		Text:       c.Delta.Content,
		TokenDelta: estimateTokens(c.Delta.Content),
	}
	if c.FinishReason != nil && *c.FinishReason != "" {
		chunk.IsFinal = true
	}
	return chunk, true
}

// NormalizeGenericChunk treats whatever sits after "data: " as
// opaque text. Used when the provider is unknown / custom.
func NormalizeGenericChunk(raw string) (StreamChunk, bool) {
	dataLine := extractSSEData(raw)
	if dataLine == "" {
		return StreamChunk{}, false
	}
	if dataLine == "[DONE]" {
		return StreamChunk{IsFinal: true}, true
	}
	return StreamChunk{Text: dataLine, TokenDelta: estimateTokens(dataLine)}, true
}

// extractSSEData pulls the "data:" line out of an SSE event.
// Multiple data: lines in one event are joined with "\n" per
// the spec.
func extractSSEData(raw string) string {
	var parts []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data:") {
			parts = append(parts, strings.TrimSpace(line[5:]))
		}
	}
	return strings.Join(parts, "\n")
}

// estimateTokens is a deliberately rough heuristic — one token
// per ~4 characters. Good enough for TotalTokens accounting
// when the provider doesn't surface a usage block.
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len(s) / 4
	if n == 0 {
		n = 1
	}
	return n
}

// ─── serialisation: StreamChunk → provider SSE ──

// ToAnthropicSSE renders a chunk in Anthropic's event format.
// The terminator is two newlines per SSE spec.
func ToAnthropicSSE(chunk StreamChunk, isLast bool) string {
	if isLast || chunk.IsFinal {
		return `event: message_stop` + "\n" +
			`data: {"type":"message_stop"}` + "\n\n"
	}
	payload, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]string{"type": "text_delta", "text": chunk.Text},
	})
	return "event: content_block_delta\n" + "data: " + string(payload) + "\n\n"
}

// ToOpenAISSE renders a chunk in OpenAI's chat-completions
// streaming format.
func ToOpenAISSE(chunk StreamChunk, isLast bool) string {
	if isLast || chunk.IsFinal {
		// OpenAI terminates with a final chunk carrying
		// finish_reason="stop" plus a sentinel [DONE].
		stop := `{"choices":[{"delta":{},"finish_reason":"stop"}]}`
		return "data: " + stop + "\n\n" + "data: [DONE]\n\n"
	}
	payload, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"delta": map[string]string{"content": chunk.Text},
		}},
	})
	return "data: " + string(payload) + "\n\n"
}

// ─── replay ──────────────────────────────────────

// ReplayStream writes a cached StreamEntry back to the response
// writer using the requested strategy. `provider` controls the
// output format (anthropic / openai / generic).
//
// Context cancellation aborts replay immediately so a client
// that disconnects mid-replay doesn't keep us sleeping.
func ReplayStream(
	ctx context.Context,
	entry *StreamEntry,
	w http.ResponseWriter,
	provider string,
	strategy ReplayStrategy,
) error {
	if entry == nil {
		return errors.New("cache: nil StreamEntry")
	}
	if strategy == "" {
		strategy = DefaultReplayStrategy
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Talyvor-Cache", "hit")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	emit := func(s string) error {
		if _, err := io.WriteString(w, s); err != nil { // nosemgrep: go.lang.security.audit.xss.no-io-writestring-to-responsewriter.no-io-writestring-to-responsewriter -- text/event-stream replay of a cached answer
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	for i, chunk := range entry.Chunks {
		// Compute pre-chunk delay according to the strategy.
		// We skip the wait before the very first chunk so the
		// client sees data immediately.
		if i > 0 {
			var wait time.Duration
			switch strategy {
			case ReplayInstant:
				wait = 0
			case ReplayRealistic:
				wait = time.Duration(chunk.DelayMs) * time.Millisecond
			case ReplayFast:
				d := time.Duration(chunk.DelayMs) * time.Millisecond
				if d > FastReplayDelayCap {
					d = FastReplayDelayCap
				}
				wait = d
			}
			if wait > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
			}
		}
		isLast := i == len(entry.Chunks)-1
		// When the cached chunk carries both content AND a
		// terminal marker, emit the content first then the
		// stop event — otherwise the last token would be lost.
		hasContent := chunk.Text != ""
		switch provider {
		case "anthropic":
			if hasContent {
				if err := emit(ToAnthropicSSE(StreamChunk{Text: chunk.Text}, false)); err != nil {
					return err
				}
			}
			if isLast || chunk.IsFinal {
				if err := emit(ToAnthropicSSE(StreamChunk{IsFinal: true}, true)); err != nil {
					return err
				}
			}
		case "openai":
			if hasContent {
				if err := emit(ToOpenAISSE(StreamChunk{Text: chunk.Text}, false)); err != nil {
					return err
				}
			}
			if isLast || chunk.IsFinal {
				if err := emit(ToOpenAISSE(StreamChunk{IsFinal: true}, true)); err != nil {
					return err
				}
			}
		default:
			if hasContent {
				if err := emit("data: " + chunk.Text + "\n\n"); err != nil {
					return err
				}
			}
			if isLast || chunk.IsFinal {
				if err := emit("data: [DONE]\n\n"); err != nil {
					return err
				}
			}
		}
	}
	// Belt-and-braces final [DONE] for callers that always
	// expect the sentinel (OpenAI clients especially).
	if provider == "openai" || provider == "" || provider == "generic" {
		if len(entry.Chunks) == 0 || !entry.Chunks[len(entry.Chunks)-1].IsFinal {
			_ = emit("data: [DONE]\n\n")
		}
	}
	return nil
}

// ─── store helpers ───────────────────────────────

// SetStream persists a StreamEntry as JSON under `key`. Returns
// an error when the marshalled payload exceeds MaxStreamEntryBytes
// — large entries are dropped so Redis memory stays bounded.
func SetStream(ctx context.Context, rdb *redis.Client, key string, entry StreamEntry, ttl time.Duration) error {
	if rdb == nil {
		return errors.New("cache: nil redis client")
	}
	buf, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("cache: marshal stream: %w", err)
	}
	if len(buf) > MaxStreamEntryBytes {
		return fmt.Errorf("cache: stream entry %d bytes exceeds %d", len(buf), MaxStreamEntryBytes)
	}
	if ttl <= 0 {
		ttl = entry.TTL
	}
	return rdb.Set(ctx, streamKeyPrefix+key, buf, ttl).Err()
}

// GetStream returns the cached StreamEntry, or nil with no error
// when the key is missing (Redis miss is not an error condition).
func GetStream(ctx context.Context, rdb *redis.Client, key string) (*StreamEntry, error) {
	if rdb == nil {
		return nil, nil
	}
	buf, err := rdb.Get(ctx, streamKeyPrefix+key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, fmt.Errorf("cache: get stream: %w", err)
	}
	var entry StreamEntry
	if err := json.Unmarshal(buf, &entry); err != nil {
		return nil, fmt.Errorf("cache: unmarshal stream: %w", err)
	}
	return &entry, nil
}

// FromNonStreaming converts a single cached response body into a
// one-chunk StreamEntry — used when a streaming request finds a
// non-streaming cache entry. The single chunk carries the whole
// body as text + IsFinal=true.
func FromNonStreaming(provider, model string, body []byte) StreamEntry {
	text := string(body)
	return StreamEntry{
		Provider: provider,
		Model:    model,
		Chunks: []StreamChunk{
			{Text: text, TokenDelta: estimateTokens(text), IsFinal: true},
		},
		TotalTokens: estimateTokens(text),
		CreatedAt:   time.Now(),
	}
}
