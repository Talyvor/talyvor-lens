package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// B50.2: no request body may exceed the limit, whether it declares its length or streams it.
func TestBodyLimitMiddleware(t *testing.T) {
	var read int
	h := BodyLimitMiddleware(16)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		read = len(b)
		if err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		}
	}))
	call := func(body []byte, chunked bool) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/anything", bytes.NewReader(body))
		if chunked {
			req.ContentLength = -1
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if code := call(make([]byte, 16), false); code != http.StatusOK || read != 16 {
		t.Fatalf("a body at the limit = %d (read %d), want 200", code, read)
	}
	read = -1
	if code := call(make([]byte, 17), false); code != http.StatusRequestEntityTooLarge || read != -1 {
		t.Fatalf("a declared oversize body = %d (handler read %d), want 413 before the handler", code, read)
	}
	if code := call(make([]byte, 1<<20), true); code != http.StatusRequestEntityTooLarge || read > 16 {
		t.Fatalf("a streamed oversize body = %d (read %d), want 413 with at most 16 bytes read", code, read)
	}
}
