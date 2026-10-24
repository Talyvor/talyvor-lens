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
	"github.com/talyvor/lens/internal/economy"
)

// B17.112 / B28.118 — WITH SEARCH THE WEB ON, A NEWS ANSWER CITES AT LEAST TWO PAGES, AND EACH OF THEM OPENS.
//
// The upstream below is Anthropic's web search on the search round — two news pages found, which it also serves, and
// what they say — and a model that answers citing them on the answer round. Through the real handler: the client's
// stream begins with a talyvor.citations frame naming both pages, each opens, the answer round was given the pages
// and what they say, and each of the two requests is one ledger row at its model's catalog price.

type b17112Upstream struct {
	mu     sync.Mutex
	bodies []map[string]any
	url    string
}

func (u *b17112Upstream) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/news/") {
			_, _ = io.WriteString(w, "<html><title>news</title></html>") // a page the search found
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.bodies = append(u.bodies, body)
		u.mu.Unlock()
		if tools, _ := json.Marshal(body["tools"]); strings.Contains(string(tools), "web_search_20250305") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"type":"message","role":"assistant","content":[`+
				`{"type":"text","text":"I'll search for that."},`+
				`{"type":"server_tool_use","id":"srvtoolu_b17112","name":"web_search","input":{"query":"central banks news today"}},`+
				`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_b17112","content":[`+
				`{"type":"web_search_result","url":"`+u.url+`/news/rates","title":"Central bank holds rates","encrypted_content":"x","page_age":"October 10, 2026"},`+
				`{"type":"web_search_result","url":"`+u.url+`/news/markets","title":"Markets close higher","encrypted_content":"x"}]},`+
				`{"type":"text","text":"The central bank held its rate at 4%.","citations":[{"type":"web_search_result_location","url":"`+u.url+`/news/rates","cited_text":"held at 4%"}]},`+
				`{"type":"text","text":" Markets rose on the news.","citations":[{"type":"web_search_result_location","url":"`+u.url+`/news/markets","cited_text":"rose"}]}],`+
				`"stop_reason":"end_turn","usage":{"input_tokens":10000,"output_tokens":100,"server_tool_use":{"web_search_requests":1}}}`)
			return
		}
		answer := jsonString("The central bank held rates at 4% [1], and markets rose [2].")
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			for _, f := range []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":10000,"output_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + answer + `}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":100}}`,
				`{"type":"message_stop"}`,
			} {
				var e struct{ Type string }
				_ = json.Unmarshal([]byte(f), &e)
				_, _ = io.WriteString(w, "event: "+e.Type+"\ndata: "+f+"\n\n")
			}
			return
		}
		for _, c := range []string{
			`{"choices":[{"delta":{"content":` + answer + `}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10000,"completion_tokens":100}}`,
			`[DONE]`,
		} {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
	}))
	u.url = srv.URL
	t.Cleanup(srv.Close)
	return srv
}

// b17112Instructions is what a request told the model beyond the conversation: Anthropic's system, or OpenAI's system messages.
func b17112Instructions(body map[string]any) string {
	if s, ok := body["system"].(string); ok {
		return s
	}
	var sb strings.Builder
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		if msg, _ := m.(map[string]any); msg["role"] == "system" {
			s, _ := msg["content"].(string)
			sb.WriteString(s)
		}
	}
	return sb.String()
}

func TestB17112_WithSearchTheWebOnANewsAnswerCitesTwoPagesThatOpen(t *testing.T) {
	search, ok := catalog.Get(webSearchModel)
	if !ok {
		t.Fatalf("%s, the search round's model, is not in the catalog", webSearchModel)
	}
	for _, tc := range []struct {
		model     string
		anthropic bool
	}{
		{"claude-haiku-4-5", true},
		{"gpt-4o", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			m, ok := catalog.Get(tc.model)
			if !ok {
				t.Fatalf("%s is not in the catalog", tc.model)
			}
			p, _, _, pool := chatProxy(t, costWireFunded, 0, economy.DefaultAgentCeilingLXC)
			up := &b17112Upstream{}
			srv := up.serve(t)
			p.openAIURL, p.anthropicURL = srv.URL+"/v1/chat/completions", srv.URL+"/v1/messages"

			q := "What is in the news today about central banks? Cite your sources. b17112"
			body := `{"model":"` + tc.model + `","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":` + jsonString(q) + `}]}`
			path := "/v1/proxy/openai/v1/chat/completions"
			if tc.anthropic {
				path = "/v1/proxy/anthropic/v1/messages"
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Talyvor-Workspace", "ws-log")
			req.Header.Set(WebSearchHeader, "on")
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

			var frame struct {
				Type      string
				Citations []struct {
					N          int
					URL, Title string
				}
			}
			data, ok := strings.CutPrefix(string(got), "event: talyvor.citations\ndata: ")
			if !ok || json.Unmarshal([]byte(data[:strings.Index(data, "\n")]), &frame) != nil {
				t.Fatalf("the stream does not start with a talyvor.citations frame: %.800s", got)
			}
			if frame.Type != "talyvor.citations" || len(frame.Citations) != 2 ||
				frame.Citations[0].N != 1 || frame.Citations[0].URL != srv.URL+"/news/rates" || frame.Citations[0].Title != "Central bank holds rates" ||
				frame.Citations[1].N != 2 || frame.Citations[1].URL != srv.URL+"/news/markets" {
				t.Errorf("citations frame = %+v; want the two pages the search found, numbered 1 and 2", frame)
			}
			for _, c := range frame.Citations {
				if res, err := http.Get(c.URL); err != nil || res.StatusCode != http.StatusOK {
					t.Errorf("cited page %s does not open: %v %v", c.URL, res, err)
				}
			}
			if !strings.Contains(string(got), "markets rose [2]") {
				t.Errorf("the answer is not in the stream: %.800s", got)
			}

			up.mu.Lock()
			bodies := up.bodies
			up.mu.Unlock()
			if len(bodies) != 2 {
				t.Fatalf("the upstream was asked %d times; want 2 (the search, then the answer)", len(bodies))
			}
			if bodies[0]["model"] != webSearchModel || !strings.Contains(lastUserText(bodies[0]["messages"]), "central banks") {
				t.Errorf("the search round asked %v %q; want %s searching the question", bodies[0]["model"], lastUserText(bodies[0]["messages"]), webSearchModel)
			}
			told := b17112Instructions(bodies[1])
			for _, want := range []string{"[1] Central bank holds rates — " + srv.URL + "/news/rates (October 10, 2026)", "[2] Markets close higher — " + srv.URL + "/news/markets",
				"The central bank held its rate at 4%. [1] Markets rose on the news. [2]", "[1] or [2]"} {
				if !strings.Contains(told, want) {
					t.Errorf("the answer round was not told %q; it was told %q", want, told)
				}
			}
			if bodies[1]["model"] != tc.model {
				t.Errorf("the answer came from %v; want the model asked for, %s", bodies[1]["model"], tc.model)
			}

			want := b1711CatalogULXC(search) + b1711CatalogULXC(m)
			rows, debited, _ := prepaidDebits(t, pool)
			if rows != 2 || debited != want {
				t.Errorf("prepaid ledger = %d row(s), %d µLXC; want 2 rows, %d µLXC — the search and the answer each charged at its model's catalog price", rows, debited, want)
			}
			// B17.265: a charge frame for the search and one for the answer, summing to their rows.
			if charges := b17265ChargeFrames(t, string(got)); len(charges) != rows || charges[0]+charges[1] != debited {
				t.Errorf("charge frames %v; want %d of them summing to the rows' %d µLXC: %.1500s", charges, rows, debited, got)
			}
		})
	}
}
