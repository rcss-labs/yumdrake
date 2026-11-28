// Package fakegithub is an in-process fake of the GitHub REST endpoints that
// internal/github calls, for end-to-end tests. It depends only on the standard
// library and carries no build tag, so it is compiled and unit-tested by
// `make test` and importable from tests built with the e2e tag.
//
// Endpoints (GET only, bearer-token authenticated):
//
//	/repos/{owner}/{repo}/commits/{branch}        {"sha": head}
//	/repos/{owner}/{repo}/branches/{branch}       200 or 404
//	/repos/{owner}/{repo}/git/trees/{sha}         recursive tree of blobs and trees; {sha} may be "commit:dir"
//	/repos/{owner}/{repo}/contents/{path}?ref=sha base64 file, wrapped like GitHub
//
// Behaviour:
//   - A commit is an in-memory snapshot of files (AddCommit) or a directory
//     tree loaded from disk (LoadDir). Branch heads are pointers to commits and
//     are moved with SetHead; heads may name a commit added later.
//   - Missing repo, branch, commit or file answers 404.
//   - A wrong or missing bearer token answers 401 (checked before 503).
//   - SetUnavailable(true) makes every endpoint answer 503, even with a
//     valid token, until cleared.
//   - Responses carry a strong ETag derived from the body; a matching
//     If-None-Match answers 304 (counted by NotModified).
//   - Requests(endpoint) counts every request that reached an endpoint,
//     including 304, 404 and 503 answers, but not 401s.
//   - All methods are safe for concurrent use.
package fakegithub

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Endpoint names accepted by Requests.
const (
	EndpointCommits  = "commits"
	EndpointBranches = "branches"
	EndpointTrees    = "trees"
	EndpointContents = "contents"
)

// Server is a running fake GitHub.
type Server struct {
	ts      *httptest.Server // nil for NewUnstarted
	handler http.Handler
	token   string

	mu          sync.Mutex
	commits     map[string]map[string]map[string][]byte // repo -> sha -> path -> content
	heads       map[string]map[string]string            // repo -> branch -> sha
	unavailable bool
	requests    map[string]int
	notModified int
}

// New starts a fake that accepts only "Authorization: Bearer <token>".
func New(token string) *Server {
	s := NewUnstarted(token)
	s.ts = httptest.NewServer(s.handler)
	return s
}

// NewUnstarted builds a fake without starting a server: the caller serves
// Handler on a listener of its own (the demo does). URL then answers "" and
// Close does nothing.
func NewUnstarted(token string) *Server {
	s := &Server{
		token:    token,
		commits:  map[string]map[string]map[string][]byte{},
		heads:    map[string]map[string]string{},
		requests: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{name}/commits/{branch...}", s.endpoint(EndpointCommits, s.handleCommit))
	mux.HandleFunc("GET /repos/{owner}/{name}/branches/{branch...}", s.endpoint(EndpointBranches, s.handleBranch))
	mux.HandleFunc("GET /repos/{owner}/{name}/git/trees/{sha}", s.endpoint(EndpointTrees, s.handleTree))
	mux.HandleFunc("GET /repos/{owner}/{name}/contents/{path...}", s.endpoint(EndpointContents, s.handleContents))
	s.handler = mux
	return s
}

// Handler is the fake's HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// URL is the base URL to give internal/github.New ("" when not started).
func (s *Server) URL() string {
	if s.ts == nil {
		return ""
	}
	return s.ts.URL
}

// Close stops the server it started, if any.
func (s *Server) Close() {
	if s.ts != nil {
		s.ts.Close()
	}
}

// AddCommit registers (or replaces) the full file tree of commit sha in repo
// ("owner/name"). Paths use forward slashes. The map is copied.
func (s *Server) AddCommit(repo, sha string, files map[string][]byte) {
	snapshot := make(map[string][]byte, len(files))
	for p, b := range files {
		snapshot[strings.TrimLeft(p, "/")] = append([]byte(nil), b...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commits[repo] == nil {
		s.commits[repo] = map[string]map[string][]byte{}
	}
	s.commits[repo][sha] = snapshot
}

// LoadDir registers commit sha from every regular file under dir on disk.
func (s *Server) LoadDir(repo, sha, dir string) error {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // p comes from walking a test-supplied directory
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		return fmt.Errorf("fakegithub: loading %s: %w", dir, err)
	}
	s.AddCommit(repo, sha, files)
	return nil
}

// SetHead points branch of repo at commit sha, creating the branch if needed.
func (s *Server) SetHead(repo, branch, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.heads[repo] == nil {
		s.heads[repo] = map[string]string{}
	}
	s.heads[repo][branch] = sha
}

// SetUnavailable makes every endpoint answer 503 while on is true.
func (s *Server) SetUnavailable(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unavailable = on
}

// Requests returns how many authenticated requests reached the endpoint
// (one of the Endpoint constants), whatever the answer.
func (s *Server) Requests(endpoint string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[endpoint]
}

// NotModified returns how many 304 answers the fake has sent.
func (s *Server) NotModified() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notModified
}

// endpoint wraps a handler with auth, counting and the availability switch.
// The handler returns a status and a JSON-encodable body; the wrapper adds the
// ETag and 304 handling for 200 answers.
func (s *Server) endpoint(name string, h func(r *http.Request) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		s.mu.Lock()
		s.requests[name]++
		down := s.unavailable
		s.mu.Unlock()
		if down {
			http.Error(w, `{"message":"Service Unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		status, payload := h(r)
		if status != http.StatusOK {
			http.Error(w, `{"message":"Not Found"}`, status)
			return
		}
		body, err := json.Marshal(payload)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sum := sha256.Sum256(body)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			s.mu.Lock()
			s.notModified++
			s.mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func repoOf(r *http.Request) string { return r.PathValue("owner") + "/" + r.PathValue("name") }

func (s *Server) head(repo, branch string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sha, ok := s.heads[repo][branch]
	return sha, ok
}

func (s *Server) files(repo, sha string) (map[string][]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.commits[repo][sha]
	return f, ok // commit maps are never mutated after AddCommit stores them
}

func (s *Server) handleCommit(r *http.Request) (int, any) {
	sha, ok := s.head(repoOf(r), r.PathValue("branch"))
	if !ok {
		return http.StatusNotFound, nil
	}
	return http.StatusOK, map[string]string{"sha": sha}
}

func (s *Server) handleBranch(r *http.Request) (int, any) {
	branch := r.PathValue("branch")
	sha, ok := s.head(repoOf(r), branch)
	if !ok {
		return http.StatusNotFound, nil
	}
	return http.StatusOK, map[string]any{"name": branch, "commit": map[string]string{"sha": sha}}
}

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

func (s *Server) handleTree(r *http.Request) (int, any) {
	// The tree reference is "commit" or "commit:dir"; the listing is relative
	// to dir, as GitHub's is.
	sha, dir, _ := strings.Cut(r.PathValue("sha"), ":")
	dir = strings.Trim(dir, "/")
	files, ok := s.files(repoOf(r), sha)
	if !ok {
		return http.StatusNotFound, nil
	}
	dirs := map[string]bool{}
	entries := []treeEntry{}
	found := dir == ""
	for full := range files {
		p := full
		if dir != "" {
			var inside bool
			if p, inside = strings.CutPrefix(full, dir+"/"); !inside {
				continue
			}
			found = true
		}
		entries = append(entries, treeEntry{Path: p, Type: "blob"})
		for d := parentDir(p); d != ""; d = parentDir(d) {
			dirs[d] = true
		}
	}
	for d := range dirs {
		entries = append(entries, treeEntry{Path: d, Type: "tree"})
	}
	if !found {
		return http.StatusNotFound, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return http.StatusOK, map[string]any{"sha": sha, "truncated": false, "tree": entries}
}

// parentDir returns the parent directory of p, or "" at the top level.
func parentDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ""
	}
	return p[:i]
}

func (s *Server) handleContents(r *http.Request) (int, any) {
	files, ok := s.files(repoOf(r), r.URL.Query().Get("ref"))
	if !ok {
		return http.StatusNotFound, nil
	}
	p := r.PathValue("path")
	data, ok := files[p]
	if !ok {
		for f := range files { // a directory: GitHub answers with a listing, not a file
			if strings.HasPrefix(f, p+"/") {
				return http.StatusOK, []map[string]string{{"path": f, "type": "file"}}
			}
		}
		return http.StatusNotFound, nil
	}
	return http.StatusOK, map[string]string{
		"type":     "file",
		"path":     p,
		"encoding": "base64",
		"content":  wrap64(base64.StdEncoding.EncodeToString(data)),
	}
}

// wrap64 wraps base64 at 60 columns with a trailing newline, as GitHub does.
func wrap64(s string) string {
	var b strings.Builder
	for len(s) > 60 {
		b.WriteString(s[:60] + "\n")
		s = s[60:]
	}
	b.WriteString(s + "\n")
	return b.String()
}
