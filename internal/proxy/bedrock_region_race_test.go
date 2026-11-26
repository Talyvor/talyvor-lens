package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// B28.244 — a request's X-Talyvor-Bedrock-Region was written into the Proxy's shared bedrockConfig, so
// concurrent Bedrock requests raced on it and one could be signed for another's Region, and the last
// Region asked for stuck for every later request. Each request now carries its Region in its own config
// value: run under -race, every upstream call is signed for the Region its own request named.
func TestHandleBedrock_ConcurrentRegionsEachSignedForTheirOwn(t *testing.T) {
	scope := regexp.MustCompile(`Credential=[^/]+/\d{8}/([a-z0-9-]+)/bedrock/`)
	regions := []string{"us-east-1", "us-west-2", "eu-central-1", "eu-west-1", "ap-northeast-1"}
	for _, stream := range []bool{false, true} {
		var (
			mu         sync.Mutex
			mismatches []string
			served     int
		)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var sent struct {
				Messages []struct {
					Content any `json:"content"`
				} `json:"messages"`
			}
			_ = json.Unmarshal(raw, &sent)
			asked := ""
			if len(sent.Messages) > 0 {
				b, _ := json.Marshal(sent.Messages[0].Content)
				for _, rg := range regions {
					if strings.Contains(string(b), "region "+rg+" ") {
						asked = rg
					}
				}
			}
			signed := ""
			if m := scope.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
				signed = m[1]
			}
			mu.Lock()
			served++
			if asked == "" || signed != asked {
				mismatches = append(mismatches, fmt.Sprintf("asked %q, signed for %q", asked, signed))
			}
			mu.Unlock()
			if stream {
				w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
				for _, ev := range []string{
					`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
					`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
					`{"type":"message_stop"}`,
				} {
					_, _ = w.Write(bedrockChunk(ev))
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"hi"}],"role":"assistant"}`)
		}))
		p := newBedrockProxy(t, srv.URL, true)

		const perRegion = 8
		var wg sync.WaitGroup
		codes := make(chan int, perRegion*len(regions))
		for i := 0; i < perRegion; i++ {
			for _, rg := range regions {
				wg.Add(1)
				go func(rg string, i int) {
					defer wg.Done()
					body := fmt.Sprintf(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"region %s request %d stream %v"}],"max_tokens":16,"stream":%v}`, rg, i, stream, stream)
					req := httptest.NewRequest(http.MethodPost, "/v1/proxy/bedrock/invoke", strings.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Talyvor-Bedrock-Region", rg)
					w := httptest.NewRecorder()
					p.HandleBedrock(w, req)
					codes <- w.Code
				}(rg, i)
			}
		}
		wg.Wait()
		srv.Close()
		close(codes)
		for c := range codes {
			if c != http.StatusOK {
				t.Errorf("stream=%v: a request answered %d, want 200", stream, c)
			}
		}
		if served != perRegion*len(regions) {
			t.Errorf("stream=%v: AWS saw %d calls, want %d", stream, served, perRegion*len(regions))
		}
		for _, m := range mismatches {
			t.Errorf("stream=%v: %s", stream, m)
		}
		if got := p.bedrockConfig.Region; got != "us-east-1" {
			t.Errorf("stream=%v: the deployment's Region became %q after per-request overrides, want it left at us-east-1", stream, got)
		}
	}
}
