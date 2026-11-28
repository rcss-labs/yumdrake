package api

import (
	"bytes"
	"io"
	"net/http"
)

// decodeOptionalBody is decodeBody for a request that may have no body at
// all, whether it says so with a zero length or is chunked and empty.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return true
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, codeInvalid, "request body is larger than 1 MB")
		return false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return true
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return decodeBody(w, r, dst)
}

// restoreRequest is the body of the restore endpoint. Every field is optional.
type restoreRequest struct {
	Services []string `json:"services"`
}

// restoreRun plans a restore of what a finished run changed. The services are
// optional: none means every service the run changed. The engine checks the
// restore permission against the run's target and writes the audit entry.
func (s *server) restoreRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		notFound(w, "run")
		return
	}
	principal, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req restoreRequest
	if !decodeOptionalBody(w, r, &req) {
		return
	}
	run, err := s.Runs.CreateRestore(r.Context(), principal, id, req.Services, viaOf(principal))
	if err != nil {
		s.engineError(w, r, err)
		return
	}
	s.respondRun(w, r, run.ID, http.StatusCreated)
}

// rerunFailed plans a new run of the services of a finished run that failed,
// were skipped or were cancelled, at the versions they had.
func (s *server) rerunFailed(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		notFound(w, "run")
		return
	}
	principal, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req struct{}
	if !decodeOptionalBody(w, r, &req) {
		return
	}
	run, err := s.Runs.CreateRerun(r.Context(), principal, id, viaOf(principal))
	if err != nil {
		s.engineError(w, r, err)
		return
	}
	s.respondRun(w, r, run.ID, http.StatusCreated)
}
