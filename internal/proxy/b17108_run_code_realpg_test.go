package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/catalog"
	"github.com/talyvor/lens/internal/coderun"
	"github.com/talyvor/lens/internal/economy"
)

// B17.108 / B28.119 — WITH RUN CODE ON, "THE 100TH PRIME" RETURNS 541, WORKED OUT BY CODE THE MODEL RAN IN THE SANDBOX.
//
// The upstream below is a model that, offered run_code, asks to run a prime sieve, and once it has the run's output
// answers with it. Through the real handler and the real sandbox (this test binary is the sandbox child, see
// TestMain): the client's stream carries a talyvor.code_run frame whose output is 541 and an answer saying 541, the
// second request carried the run's output back to the model, and each of the two requests is one ledger row at the
// model's catalog price.

const b17108Code = `const p = []; for (let n = 2; p.length < 100; n++) if (p.every((q) => n % q)) p.push(n); console.log(p[99])`

type b17108Upstream struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (u *b17108Upstream) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.bodies = append(u.bodies, body)
		u.mu.Unlock()
		ran := b17108ToolOutput(body)
		args := `{"code":` + jsonString(b17108Code) + `}`
		half := len(args) / 2
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			frames := `{"type":"message_start","message":{"usage":{"input_tokens":10000,"output_tokens":1}}}` + "\n" +
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n"
			if ran == "" {
				frames += `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I'll work it out."}}` + "\n" +
					`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_b17108","name":"run_code","input":{}}}` + "\n" +
					`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":` + jsonString(args[:half]) + `}}` + "\n" +
					`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":` + jsonString(args[half:]) + `}}` + "\n" +
					`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":100}}` + "\n"
			} else {
				frames += `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + jsonString("The 100th prime is "+strings.TrimSpace(ran)+".") + `}}` + "\n" +
					`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":100}}` + "\n"
			}
			for _, f := range strings.Split(strings.TrimSpace(frames+`{"type":"message_stop"}`), "\n") {
				var e struct{ Type string }
				_ = json.Unmarshal([]byte(f), &e)
				_, _ = io.WriteString(w, "event: "+e.Type+"\ndata: "+f+"\n\n")
			}
			return
		}
		chunks := []string{}
		if ran == "" {
			chunks = append(chunks,
				`{"choices":[{"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"call_b17108","type":"function","function":{"name":"run_code","arguments":""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":`+jsonString(args[:half])+`}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":`+jsonString(args[half:])+`}}]}}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		} else {
			chunks = append(chunks, `{"choices":[{"delta":{"content":`+jsonString("The 100th prime is "+strings.TrimSpace(ran)+".")+`}}]}`)
		}
		for _, c := range append(chunks, `{"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":100}}`, `[DONE]`) {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// b17108ToolOutput is the run's output a request carries back to the model, "" when it carries none.
func b17108ToolOutput(body map[string]any) string {
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		return ""
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	if last["role"] == "tool" {
		s, _ := last["content"].(string)
		return s
	}
	blocks, _ := last["content"].([]any)
	for _, b := range blocks {
		if block, _ := b.(map[string]any); block["type"] == "tool_result" {
			s, _ := block["content"].(string)
			return s
		}
	}
	return ""
}

// b17265ChargeFrames is the charged_ulxc of every talyvor.charge frame in a stream, in order.
func b17265ChargeFrames(t *testing.T, stream string) []int64 {
	t.Helper()
	var out []int64
	for _, part := range strings.Split(stream, "event: "+chargeFrame+"\ndata: ")[1:] {
		var f struct {
			Charged int64 `json:"charged_ulxc"`
		}
		if err := json.Unmarshal([]byte(part[:strings.Index(part, "\n")]), &f); err != nil {
			t.Fatalf("charge frame %.200q: %v", part, err)
		}
		out = append(out, f.Charged)
	}
	return out
}

func TestB17108_WithRunCodeOnTheHundredthPrimeIs541WorkedOutInTheSandbox(t *testing.T) {
	runner, err := coderun.NewRunner()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		model     string
		anthropic bool
		tools     string
	}{
		{"claude-haiku-4-5", true, `[{"name":"agent_spend","description":"What an agent spent","input_schema":{"type":"object"}}]`},
		{"gpt-4o", false, b17104Tools},
	} {
		t.Run(tc.model, func(t *testing.T) {
			m, ok := catalog.Get(tc.model)
			if !ok {
				t.Fatalf("%s is not in the catalog", tc.model)
			}
			p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
			p.SetCodeRunner(runner)
			up := &b17108Upstream{}
			srv := up.serve(t)
			p.openAIURL, p.anthropicURL = srv.URL+"/v1/chat/completions", srv.URL+"/v1/messages"

			q := jsonString("What is the 100th prime? Run code to work it out. b17108 " + strings.Repeat("p", 400))
			body := `{"model":"` + tc.model + `","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":` + q + `}],"tools":` + tc.tools + `}`
			path := "/v1/proxy/openai/v1/chat/completions"
			if tc.anthropic {
				path = "/v1/proxy/anthropic/v1/messages"
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Talyvor-Workspace", "ws-log")
			req.Header.Set(RunCodeHeader, "on")
			req.Header.Set(reportChargeHeader, "true")
			req = req.WithContext(auth.WithAuthContext(req.Context(), sessionKeyAuthContext(t, "ws-log")))
			rec := newFlushRecorder()
			if tc.anthropic {
				p.HandleAnthropic(rec, req)
			} else {
				p.HandleOpenAI(rec, req)
			}
			res := rec.Result()
			got, _ := io.ReadAll(res.Body)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %.800s", res.StatusCode, got)
			}

			var run struct {
				Type, Language, Code, Stdout string
				ExitCode                     int `json:"exit_code"`
			}
			frame, ok := strings.CutPrefix(string(got), "event: talyvor.code_run\ndata: ")
			if !ok || json.Unmarshal([]byte(frame[:strings.Index(frame, "\n")]), &run) != nil {
				t.Fatalf("the stream does not start with a talyvor.code_run frame: %.800s", got)
			}
			if run.Type != "talyvor.code_run" || run.Language != "javascript" || run.Code != b17108Code || run.Stdout != "541\n" || run.ExitCode != 0 {
				t.Errorf("code_run frame = %+v; want the model's code, printing 541, exit 0", run)
			}
			if !strings.Contains(string(got), "The 100th prime is 541.") {
				t.Errorf("the answer does not say 541: %.800s", got)
			}

			up.mu.Lock()
			bodies := up.bodies
			up.mu.Unlock()
			if len(bodies) != 2 {
				t.Fatalf("the model was asked %d times; want 2 (the question, then the run's output)", len(bodies))
			}
			if offered, _ := json.Marshal(bodies[0]["tools"]); !strings.Contains(string(offered), `"run_code"`) || !strings.Contains(string(offered), `"agent_spend"`) {
				t.Errorf("first request's tools = %s; want the client's agent_spend and Lens's run_code", offered)
			}
			if out := b17108ToolOutput(bodies[1]); out != "541\n" {
				t.Errorf("the second request carried the run's output as %q; want \"541\\n\"", out)
			}

			want := b1711CatalogULXC(m)
			rows, debited, _ := prepaidDebits(t, pool)
			if rows != 2 || debited != 2*want {
				t.Errorf("prepaid ledger = %d row(s), %d µLXC; want 2 rows of %d µLXC — each request to the model charged", rows, debited, want)
			}
			// B17.265: a charge frame for each request charged, summing to its rows.
			if charges := b17265ChargeFrames(t, string(got)); len(charges) != rows || charges[0]+charges[1] != debited {
				t.Errorf("charge frames %v; want %d of them summing to the rows' %d µLXC: %.1500s", charges, rows, debited, got)
			}
		})
	}
}
