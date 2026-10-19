package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/talyvor/lens/internal/cache"
	"github.com/talyvor/lens/internal/coderun"
)

// B28.119 — code execution in a sandbox. With RunCodeHeader "on", Lens offers the model a run_code tool. Each time
// the model calls it, Lens runs the code in its sandbox (internal/coderun), gives the model what it printed and asks
// again, until the model answers. Every round is an ordinary request through serve — gated, held and charged on its
// own — and the client gets the last round's answer as it came back. A streaming client gets a talyvor.code_run frame
// for each run as soon as it is back (talyvor-suite apps/web chatStream.ts CODE_RUN_FRAME), then that answer.

// RunCodeHeader "on" lets the model run code in Lens's sandbox while it answers.
const RunCodeHeader = "X-Talyvor-Run-Code"

const (
	runCodeTool     = "run_code"
	maxCodeRounds   = 4 // rounds that may run code; the round after them is asked to answer with what it has
	maxRunsPerRound = 4 // runs one answer may ask for; the rest are answered "not run"
	runCodeToolDesc = "Runs JavaScript and returns what it printed. Use it to work out anything that needs computing — " +
		"arithmetic, counting, searching, checking — rather than answering from memory, then answer with the result. " +
		"Print with console.log; if nothing is printed, the value of the last expression is shown. There is no network, " +
		"file system, require or import, and a run stops after 5 seconds."
)

type codeRunner interface {
	Run(ctx context.Context, code string) coderun.Result
}

// SetCodeRunner turns Run code on for requests that ask for it. Without one, RunCodeHeader is ignored.
func (p *Proxy) SetCodeRunner(r codeRunner) { p.codeRunner = r }

// runCodeAnswer is a code-running request's cache key, read in its first round, and the answer its latest round
// would have stored (B17.225). No round is stored on its own — one may be a call to run code, and a streamed one is
// stored as its text alone — so the finished answer is stored under the question as asked, and the same question
// asked again is served it from the first round's cache read.
type runCodeAnswer struct {
	keyed                                                        bool
	provider, model, cachePrompt, prompt, reqFP, wsID, requestID string
	turn                                                         cache.Turn
	answer                                                       []byte
}

type runCodeAnswerKey struct{}

func runCodeAnswerOf(ctx context.Context) *runCodeAnswer {
	rc, _ := ctx.Value(runCodeAnswerKey{}).(*runCodeAnswer)
	return rc
}

// toolCall is one tool the model asked for in an answer.
type toolCall struct{ ID, Name, Args string }

func (p *Proxy) serveRunningCode(w http.ResponseWriter, r *http.Request, cfg providerConfig) {
	raw, err := readLimitedBody(r, maxBodyBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds 4MB limit — upload a large document to POST /v1/documents (up to 25 MB) and reference it by id")
			return
		}
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // a number in the body goes on as it came
	if dec.Decode(&req) != nil || req == nil {
		p.serve(w, runCodeRound(r, raw, 0), cfg) // serve says what is wrong with the body
		return
	}
	anthropic := cfg.ProviderName() == "anthropic"
	streaming := req["stream"] == true
	msgs, _ := req["messages"].([]any)
	tools, _ := req["tools"].([]any)
	req["tools"] = append(tools, runCodeToolDef(anthropic))

	base := r.Context()
	rc := &runCodeAnswer{}
	r = r.WithContext(context.WithValue(base, runCodeAnswerKey{}, rc))
	committed := false // a streaming client's response has begun
	for n := 0; ; n++ {
		if n == maxCodeRounds {
			req["tool_choice"] = any("none")
			if anthropic {
				req["tool_choice"] = map[string]any{"type": "none"}
			}
		}
		req["messages"] = msgs
		body, err := json.Marshal(req)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		rec := httptest.NewRecorder()
		rc.answer = nil
		p.serve(rec, runCodeRound(r, body, n), cfg)
		text, calls := answerOf(rec.Body.Bytes(), anthropic, streaming)
		// shortcut: an answer calling run_code and one of the client's own tools goes to the client whole, run_code
		// included; split the calls if a client ever offers both and the model asks for both at once.
		if rec.Code != http.StatusOK || n == maxCodeRounds || !onlyRunCode(calls) {
			writeRunCodeAnswer(w, rec, committed)
			if rec.Code == http.StatusOK && len(calls) == 0 && rc.keyed && rc.answer != nil {
				sctx, cancel := detachBounded(base) // B39.4
				p.storeAnswer(withCacheStoreHeader(sctx, r), rc.provider, rc.model, rc.cachePrompt,
					rc.prompt, rc.reqFP, rc.turn, rc.wsID, rc.requestID, rc.answer)
				cancel()
			}
			return
		}
		if streaming && !committed {
			// The stream begins at the first run, so each run is on screen while the model carries on, and a slow
			// round never leaves a relay waiting for the response to start.
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(http.StatusOK)
			committed = true
		}
		var results []any
		reply := func(id string, run coderun.Result) {
			if anthropic {
				results = append(results, map[string]any{"type": "tool_result", "tool_use_id": id, "content": runOutput(run), "is_error": run.ExitCode != 0})
			} else {
				results = append(results, map[string]any{"role": "tool", "tool_call_id": id, "content": runOutput(run)})
			}
		}
		for i, c := range calls {
			if i >= maxRunsPerRound {
				reply(c.ID, coderun.Result{Stderr: "Not run: one answer runs at most " + strconv.Itoa(maxRunsPerRound) + " pieces of code.", ExitCode: 1})
				continue
			}
			var in struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal([]byte(c.Args), &in)
			run := p.codeRunner.Run(r.Context(), in.Code)
			if committed {
				frame, _ := json.Marshal(struct {
					Type string `json:"type"`
					coderun.Result
				}{"talyvor.code_run", run})
				writeFrame(w, "talyvor.code_run", frame)
			}
			reply(c.ID, run)
		}
		msgs = append(msgs, assistantTurn(anthropic, text, calls))
		if anthropic {
			msgs = append(msgs, map[string]any{"role": "user", "content": results})
		} else {
			msgs = append(msgs, results...)
		}
	}
}

// runCodeRound is the request for one round: the original, with body, minus RunCodeHeader so serve serves it.
func runCodeRound(r *http.Request, body []byte, n int) *http.Request {
	rr := r.Clone(r.Context())
	rr.Body = io.NopCloser(bytes.NewReader(body))
	rr.ContentLength = int64(len(body))
	rr.Header.Del(RunCodeHeader)
	if n > 0 {
		// The first round reads the cache as the client asked; a later one carries a run's output, never asked before.
		rr.Header.Set(CacheBypassHeader, "bypass")
		// Each round is its own request: a repeated Idempotency-Key would be charged once, as a retry.
		for _, h := range []string{"Idempotency-Key", "X-Talyvor-Request-ID"} {
			if v := rr.Header.Get(h); v != "" {
				rr.Header.Set(h, v+"-run"+strconv.Itoa(n))
			}
		}
	}
	return rr
}

func runCodeToolDef(anthropic bool) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"code": map[string]any{"type": "string", "description": "The JavaScript to run."}},
		"required":   []any{"code"},
	}
	if anthropic {
		return map[string]any{"name": runCodeTool, "description": runCodeToolDesc, "input_schema": schema}
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": runCodeTool, "description": runCodeToolDesc, "parameters": schema}}
}

func onlyRunCode(calls []toolCall) bool {
	for _, c := range calls {
		if c.Name != runCodeTool {
			return false
		}
	}
	return len(calls) > 0
}

// answerOf reads the text and the tool calls of one round's answer, as Anthropic or OpenAI, streamed or not.
func answerOf(out []byte, anthropic, streaming bool) (string, []toolCall) {
	var text strings.Builder
	var calls []toolCall
	if !streaming {
		if anthropic {
			var m struct {
				Content []struct {
					Type, Text, ID, Name string
					Input                json.RawMessage
				}
			}
			_ = json.Unmarshal(out, &m)
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "tool_use":
					calls = append(calls, toolCall{b.ID, b.Name, string(b.Input)})
				}
			}
			return text.String(), calls
		}
		var m struct {
			Choices []struct {
				Message struct {
					Content   string
					ToolCalls []struct {
						ID       string
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				}
			}
		}
		_ = json.Unmarshal(out, &m)
		if len(m.Choices) > 0 {
			text.WriteString(m.Choices[0].Message.Content)
			for _, tc := range m.Choices[0].Message.ToolCalls {
				calls = append(calls, toolCall{tc.ID, tc.Function.Name, tc.Function.Arguments})
			}
		}
		return text.String(), calls
	}
	at := map[int]int{} // a streamed call's index → its place in calls
	for _, line := range strings.Split(string(out), "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if anthropic {
			var e struct {
				Type         string
				Index        int
				ContentBlock struct{ Type, Text, ID, Name string } `json:"content_block"`
				Delta        struct {
					Type, Text  string
					PartialJSON string `json:"partial_json"`
				}
			}
			if json.Unmarshal([]byte(data), &e) != nil {
				continue
			}
			switch {
			case e.Type == "content_block_start" && e.ContentBlock.Type == "tool_use":
				at[e.Index] = len(calls)
				calls = append(calls, toolCall{ID: e.ContentBlock.ID, Name: e.ContentBlock.Name})
			case e.Type == "content_block_start":
				text.WriteString(e.ContentBlock.Text)
			case e.Type == "content_block_delta" && e.Delta.Type == "input_json_delta":
				if i, ok := at[e.Index]; ok {
					calls[i].Args += e.Delta.PartialJSON
				}
			case e.Type == "content_block_delta":
				text.WriteString(e.Delta.Text)
			}
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content   string
					ToolCalls []struct {
						Index    int
						ID       string
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				}
			}
		}
		if json.Unmarshal([]byte(data), &c) != nil || len(c.Choices) == 0 {
			continue
		}
		d := c.Choices[0].Delta
		text.WriteString(d.Content)
		for _, tc := range d.ToolCalls {
			i, ok := at[tc.Index]
			if !ok {
				i = len(calls)
				at[tc.Index] = i
				calls = append(calls, toolCall{})
			}
			if tc.ID != "" {
				calls[i].ID = tc.ID
			}
			if tc.Function.Name != "" {
				calls[i].Name = tc.Function.Name
			}
			calls[i].Args += tc.Function.Arguments
		}
	}
	return text.String(), calls
}

// assistantTurn is the model's answer that asked to run code, as the next round sends it back.
func assistantTurn(anthropic bool, text string, calls []toolCall) map[string]any {
	args := func(c toolCall) string {
		if !json.Valid([]byte(c.Args)) {
			return "{}"
		}
		return c.Args
	}
	if anthropic {
		var content []any
		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, c := range calls {
			content = append(content, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": json.RawMessage(args(c))})
		}
		return map[string]any{"role": "assistant", "content": content}
	}
	tcs := make([]any, len(calls))
	for i, c := range calls {
		tcs[i] = map[string]any{"id": c.ID, "type": "function", "function": map[string]any{"name": c.Name, "arguments": args(c)}}
	}
	turn := map[string]any{"role": "assistant", "content": nil, "tool_calls": tcs}
	if text != "" {
		turn["content"] = text
	}
	return turn
}

// runOutput is what the model is told a run printed.
func runOutput(run coderun.Result) string {
	out := run.Stdout
	if run.Stderr != "" {
		out += "\nError: " + run.Stderr
	}
	if strings.TrimSpace(out) == "" {
		return "(It printed nothing.)"
	}
	return out
}

// writeRunCodeAnswer sends the client the last round as it came back. On a stream already begun, a refusal can no
// longer be a status, so it is an error frame, which both stream readers know.
func writeRunCodeAnswer(w http.ResponseWriter, rec *httptest.ResponseRecorder, committed bool) {
	switch {
	case !committed:
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes()) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- replays a recorded answer with the recorder's own headers (JSON or event stream)
	case rec.Code == http.StatusOK:
		_, _ = w.Write(rec.Body.Bytes()) // nosemgrep: go.lang.security.audit.xss.no-direct-write-to-responsewriter.no-direct-write-to-responsewriter -- replays a recorded answer with the recorder's own headers (JSON or event stream)
	default:
		var refused struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &refused) != nil || refused.Error.Message == "" {
			refused.Error.Message = http.StatusText(rec.Code) + ": " + strings.TrimSpace(rec.Body.String())
		}
		frame, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": refused.Error.Message}})
		writeFrame(w, "error", frame)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeFrame(w http.ResponseWriter, event string, data []byte) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data) // nosemgrep: go.lang.security.audit.xss.no-fprintf-to-responsewriter.no-fprintf-to-responsewriter -- text/event-stream frame; event is a constant and data is JSON
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
