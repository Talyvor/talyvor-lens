package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/povi"
)

// B42.2: a PoVI receipt named in a request must be the caller's workspace's before anything happens.

type b422Processor struct{ got []povi.Receipt }

func (p *b422Processor) Process(_ context.Context, r povi.Receipt) (povi.ProcessResult, error) {
	p.got = append(p.got, r)
	return povi.ProcessResult{Verified: true}, nil
}

type b422Nodes map[string]string

func (n b422Nodes) WorkspaceForNode(_ context.Context, id string) (string, error) {
	if ws, ok := n[id]; ok {
		return ws, nil
	}
	return "", errors.New("node not found")
}

func TestPoviReceiptSubmit_OnlyTheCallersWorkspaceAndNode(t *testing.T) {
	proc := &b422Processor{}
	r := chi.NewRouter()
	r.Post("/v1/workspaces/{wsID}/povi/receipts", newPoviReceiptSubmitHandler(proc, b422Nodes{"node-a": "ws-a", "node-b": "ws-b"}))
	submit := func(body string) int {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws-b/povi/receipts", strings.NewReader(body)))
		return w.Code
	}

	for name, body := range map[string]string{
		"ws-a's workspace in the body": `{"request_id":"r1","node_id":"node-b","workspace_id":"ws-a"}`,
		"ws-a's node":                  `{"request_id":"r2","node_id":"node-a","workspace_id":"ws-b"}`,
		"a node that does not exist":   `{"request_id":"r3","node_id":"node-x","workspace_id":"ws-b"}`,
	} {
		if code := submit(body); code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, code)
		}
	}
	if len(proc.got) != 0 {
		t.Fatalf("refused receipts reached the processor: %+v", proc.got)
	}
	if code := submit(`{"request_id":"r4","node_id":"node-b","workspace_id":"ws-b"}`); code != http.StatusOK || len(proc.got) != 1 {
		t.Errorf("own receipt: status %d, processed %d, want 200 and 1", code, len(proc.got))
	}
}

type b422Receipts map[string]povi.StoredReceipt

func (s b422Receipts) GetReceipt(_ context.Context, id string) (*povi.StoredReceipt, error) {
	if r, ok := s[id]; ok {
		return &r, nil
	}
	return nil, nil
}

type b422Challenger struct{ got []string }

func (c *b422Challenger) Challenge(_ context.Context, rec povi.StoredReceipt) (*povi.Challenge, error) {
	c.got = append(c.got, rec.RequestID)
	return &povi.Challenge{RequestID: rec.RequestID}, nil
}

func TestChallengeIssue_OtherWorkspacesReceiptIs404(t *testing.T) {
	ch := &b422Challenger{}
	h := newChallengeIssueHandler(b422Receipts{
		"req-a": {RequestID: "req-a", WorkspaceID: "ws-a"},
		"req-b": {RequestID: "req-b", WorkspaceID: "ws-b"},
	}, ch)
	issue := func(id string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/povi/challenges/issue", strings.NewReader(`{"request_id":"`+id+`"}`))
		req = req.WithContext(auth.WithAuthContext(req.Context(), &auth.AuthContext{WorkspaceID: "ws-b"}))
		w := httptest.NewRecorder()
		h(w, req)
		return w.Code
	}
	for _, id := range []string{"req-a", "req-missing"} {
		if code := issue(id); code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", id, code)
		}
	}
	if len(ch.got) != 0 {
		t.Fatalf("challenged another workspace's receipt: %v", ch.got)
	}
	if code := issue("req-b"); code != http.StatusOK || len(ch.got) != 1 {
		t.Errorf("own receipt: status %d, challenged %v, want 200 and [req-b]", code, ch.got)
	}
}
