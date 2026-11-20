package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/economy"
)

// withTask lets a payment an agent asks for carry its task (B62.2): "task" in the body, one line in the agent's own
// words of what the payment is for. It is cleaned (economy.CleanTask) and carried to the payment and the approval it
// files in the paying workspace — the route's wsID, else the caller's — and the body is read on by h as it came, so h
// never sees the task and nothing it decides can read it. A task over economy.TaskMax characters is refused in words
// before anything is judged. Mounted as r.With(withTask).Post(…) on each route an agent pays on.
func withTask(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 1<<20))
		if err != nil {
			writeJSONErr(w, http.StatusRequestEntityTooLarge, "the body is too large")
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var in struct {
			Task *string `json:"task"`
		}
		if json.Unmarshal(body, &in) != nil || in.Task == nil {
			h.ServeHTTP(w, req) // not a JSON object, or no task: h answers it as it always has
			return
		}
		task, err := economy.CleanTask(*in.Task)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ws := chi.URLParam(req, "wsID")
		if a := auth.GetAuthContext(req.Context()); ws == "" && a != nil {
			ws = a.WorkspaceID
		}
		h.ServeHTTP(w, req.WithContext(economy.WithAgentTask(req.Context(), ws, task)))
	})
}
