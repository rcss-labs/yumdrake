//go:build integration

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/session"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// send runs one request through the handler. Zero-value arguments are left
// out: no bearer token, no cookie, no Origin.
type sent struct {
	method, path, bearer, sessionID, origin string
	body                                    any
}

func (h *harness) send(r sent) *httptest.ResponseRecorder {
	h.t.Helper()
	var reader *bytes.Reader
	if r.body != nil {
		raw, err := json.Marshal(r.body)
		if err != nil {
			h.t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(r.method, r.path, reader)
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.sessionID != "" {
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: r.sessionID})
	}
	if r.origin != "" {
		req.Header.Set("Origin", r.origin)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	h.bodiesMu.Lock()
	h.bodies = append(h.bodies, rec.Body.String())
	h.bodiesMu.Unlock()
	return rec
}

// failure describes a response without printing its body, which could hold a
// credential: only the status and the error code.
func failure(rec *httptest.ResponseRecorder) string {
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return http.StatusText(rec.Code) + " (" + code + ")"
}

func errorCodeOf(rec *httptest.ResponseRecorder) string {
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func (h *harness) expect(rec *httptest.ResponseRecorder, status int, code string) {
	h.t.Helper()
	if rec.Code != status || (code != "" && errorCodeOf(rec) != code) {
		h.t.Fatalf("got status %d %s, want %d %s", rec.Code, failure(rec), status, code)
	}
}

func TestNoCredentialsIs401(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/api/v1/folders", "/api/v1/me", "/api/v1/no-such-route"} {
		rec := h.send(sent{method: "GET", path: path})
		h.expect(rec, http.StatusUnauthorized, "unauthorized")
	}
	// A cookie with a well-formed but unknown id and a malformed bearer token
	// are no better than nothing.
	h.expect(h.send(sent{method: "GET", path: "/api/v1/folders", sessionID: validLookingSessionID()}), http.StatusUnauthorized, "unauthorized")
	h.expect(h.send(sent{method: "GET", path: "/api/v1/folders", bearer: "ymd_not-a-token"}), http.StatusUnauthorized, "unauthorized")
}

func validLookingSessionID() string { return strings.Repeat("A", 43) }

func TestBearerTokenAuthenticates(t *testing.T) {
	h := newHarness(t)
	user := h.newUser("bearer")
	token, _ := h.newToken(user.ID)

	rec := h.send(sent{method: "GET", path: "/api/v1/me", bearer: token})
	h.expect(rec, http.StatusOK, "")
	var me struct {
		ID     int64  `json:"id"`
		Email  string `json:"email"`
		Name   string `json:"name"`
		Admin  bool   `json:"admin"`
		Grants []any  `json:"grants"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || me.ID != user.ID || me.Email != user.Email || me.Admin || me.Grants == nil {
		t.Fatalf("/me for a plain user = %+v (decode error %v)", me, err)
	}

	// A bearer request is not a cookie request: no SameOrigin check, even with a
	// foreign Origin, and a valid cookie next to it is ignored.
	h.grant(user.ID, "manage_jobs")
	rec = h.send(sent{method: "POST", path: "/api/v1/folders", bearer: token, origin: "https://evil.test", body: map[string]string{"name": "from-cli"}})
	h.expect(rec, http.StatusCreated, "")

	cookieOfAdmin := h.newSession(h.admin.ID)
	rec = h.send(sent{method: "GET", path: "/api/v1/me", bearer: "ymd_wrong", sessionID: cookieOfAdmin})
	h.expect(rec, http.StatusUnauthorized, "unauthorized")
}

func TestRevokedTokenIs401(t *testing.T) {
	h := newHarness(t)
	user := h.newUser("revoked")
	token, tok := h.newToken(user.ID)
	h.expect(h.send(sent{method: "GET", path: "/api/v1/me", bearer: token}), http.StatusOK, "")
	if err := store.RevokeToken(context.Background(), h.pool, tok.ID, user.ID, time.Now()); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	h.expect(h.send(sent{method: "GET", path: "/api/v1/me", bearer: token}), http.StatusUnauthorized, "unauthorized")
}

func TestSessionCookieAuthenticates(t *testing.T) {
	h := newHarness(t)
	user := h.newUser("cookie")
	id := h.newSession(user.ID)
	rec := h.send(sent{method: "GET", path: "/api/v1/me", sessionID: id})
	h.expect(rec, http.StatusOK, "")
	if !strings.Contains(rec.Body.String(), user.Email) {
		t.Fatalf("/me did not return the signed-in user (%s)", failure(rec))
	}
	h.expect(h.send(sent{method: "GET", path: "/api/v1/jobs", sessionID: id}), http.StatusOK, "")
}

func TestCookiePostFromOtherOriginIsForbidden(t *testing.T) {
	h := newHarness(t)
	id := h.newSession(h.admin.ID)
	body := map[string]string{"name": "csrf-folder"}

	for _, origin := range []string{"https://evil.test", "null", "http://yumdrake.test"} {
		rec := h.send(sent{method: "POST", path: "/api/v1/folders", sessionID: id, origin: origin, body: body})
		h.expect(rec, http.StatusForbidden, "forbidden")
	}
	// No Origin and no Sec-Fetch-Site: not provably same-origin either.
	h.expect(h.send(sent{method: "POST", path: "/api/v1/folders", sessionID: id, body: body}), http.StatusForbidden, "forbidden")

	if list := h.mustDo("GET", "/api/v1/folders", nil, http.StatusOK); len(list["folders"].([]any)) != 0 {
		t.Fatalf("a forbidden request created a folder: %v", list["folders"])
	}
	// Reads do not need an Origin, and the right Origin may write.
	h.expect(h.send(sent{method: "GET", path: "/api/v1/folders", sessionID: id, origin: "https://evil.test"}), http.StatusOK, "")
	h.expect(h.send(sent{method: "POST", path: "/api/v1/folders", sessionID: id, origin: publicURL, body: body}), http.StatusCreated, "")
}

func TestDisabledUserIsRefusedAndSessionDeleted(t *testing.T) {
	h := newHarness(t)
	user := h.newUser("disabled")
	id := h.newSession(user.ID)
	h.expect(h.send(sent{method: "GET", path: "/api/v1/me", sessionID: id}), http.StatusOK, "")

	if err := store.SetDisabled(context.Background(), h.pool, user.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	h.expect(h.send(sent{method: "GET", path: "/api/v1/me", sessionID: id}), http.StatusUnauthorized, "unauthorized")
	if _, err := h.sessions.Get(context.Background(), id); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("session after the refusal: Get = %v, want ErrNoSession", err)
	}
	// Enabling the user again does not bring the old session back.
	if err := store.SetDisabled(context.Background(), h.pool, user.ID, false); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	h.expect(h.send(sent{method: "GET", path: "/api/v1/me", sessionID: id}), http.StatusUnauthorized, "unauthorized")
}

func TestDisabledUsersTokenStopsAtOnce(t *testing.T) {
	h := newHarness(t)
	user := h.newUser("tokendisabled")
	token, _ := h.newToken(user.ID)
	h.expect(h.send(sent{method: "GET", path: "/api/v1/me", bearer: token}), http.StatusOK, "")
	if err := store.SetDisabled(context.Background(), h.pool, user.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	h.expect(h.send(sent{method: "GET", path: "/api/v1/me", bearer: token}), http.StatusUnauthorized, "unauthorized")
}

func TestValkeyDownCookieIs503AndTokenStillWorks(t *testing.T) {
	h := newHarness(t)
	shortenValkeyTimeout(t)
	dead := openValkey(t, "127.0.0.1:1")
	h.rebuild(func(d *Deps) { d.Sessions = session.NewManager(dead, time.Now); d.Valkey = dead })

	rec := h.send(sent{method: "GET", path: "/api/v1/me", sessionID: validLookingSessionID()})
	h.expect(rec, http.StatusServiceUnavailable, "unavailable")
	if !strings.Contains(rec.Body.String(), "Sign-in is temporarily unavailable") {
		t.Errorf("message of the 503 = %s", failure(rec))
	}
	h.expect(h.send(sent{method: "GET", path: "/api/v1/me", bearer: h.adminToken}), http.StatusOK, "")
}

func TestRequireRefusesWithoutGrantAndAllowsWithIt(t *testing.T) {
	h := newHarness(t)
	user := h.newUser("plain")
	token, _ := h.newToken(user.ID)
	folder := map[string]string{"name": "Platform"}

	h.expect(h.send(sent{method: "POST", path: "/api/v1/folders", bearer: token, body: folder}), http.StatusForbidden, "forbidden")
	h.expect(h.send(sent{method: "GET", path: "/api/v1/jobs", bearer: token}), http.StatusOK, "")
	h.expect(h.send(sent{method: "GET", path: "/api/v1/admin/github-connections", bearer: token}), http.StatusForbidden, "forbidden")
	h.expect(h.send(sent{method: "PUT", path: "/api/v1/admin/inventory-source", bearer: token,
		body: map[string]string{"repo": invRepo, "branch": "main", "path": invPath}}), http.StatusForbidden, "forbidden")
	h.expect(h.send(sent{method: "POST", path: "/api/v1/inventory/sync", bearer: token}), http.StatusForbidden, "forbidden")

	h.grant(user.ID, "manage_jobs")
	rec := h.send(sent{method: "POST", path: "/api/v1/folders", bearer: token, body: folder})
	h.expect(rec, http.StatusCreated, "")
	// manage_jobs does not carry over to connections.
	h.expect(h.send(sent{method: "GET", path: "/api/v1/admin/github-connections", bearer: token}), http.StatusForbidden, "forbidden")

	entries := h.auditEntries()
	if len(entries) != 1 || entries[0].Action != "folder.create" || entries[0].Actor != user.Email ||
		entries[0].ActorID == nil || *entries[0].ActorID != user.ID {
		t.Fatalf("audit entries = %+v, want one folder.create by %s", entries, user.Email)
	}
}

func TestBootstrapTokenNoLongerWorks(t *testing.T) {
	h := newHarness(t)
	// What used to be the shared secret of every route is now just a bad token.
	for _, old := range []string{"test-bootstrap-token-0123456789-abcdef", "e2e-bootstrap-token-0123456789abcdef"} {
		h.expect(h.send(sent{method: "GET", path: "/api/v1/folders", bearer: old}), http.StatusUnauthorized, "unauthorized")
	}
}

// Every Plan 2 write leaves an audit entry made by the caller, and none of
// them holds a secret.
func TestPlan2WritesRecordAuditEntries(t *testing.T) {
	h := newHarness(t)
	h.pushInventory("c1")
	h.pushJob("j1")
	h.setInventorySource()
	folder := h.createFolder("Backends")
	other := h.createFolder("Frontends")
	job := h.registerJob(folder)
	jobID := int64(job["id"].(float64))
	h.mustDo("PATCH", "/api/v1/jobs/"+itoa(jobID), map[string]any{"folder_id": other}, http.StatusOK)
	const secret = "ghp_audit-must-not-hold-this"
	h.mustDo("POST", "/api/v1/admin/github-connections", map[string]any{"owner": "acme", "method": "token", "token": secret}, http.StatusCreated)

	want := []string{"inventory.source_set", "folder.create", "folder.create", "job.register", "job.move", "connection.create"}
	if got := h.auditActions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit actions = %v, want %v", got, want)
	}
	for _, e := range h.auditEntries() {
		if e.Actor != h.admin.Email || e.ActorID == nil || *e.ActorID != h.admin.ID {
			t.Errorf("entry %s has actor %q, want the admin", e.Action, e.Actor)
		}
		if strings.Contains(string(e.Detail), secret) || strings.Contains(strings.ToLower(string(e.Detail)), "private_key") {
			t.Errorf("entry %s detail holds a secret-looking value", e.Action)
		}
	}
}

// A write whose audit entry cannot be stored does not happen.
func TestWriteIsUndoneWhenItsAuditEntryFails(t *testing.T) {
	h := newHarness(t)
	if _, err := h.pool.Exec(context.Background(), "ALTER TABLE audit_log ADD CONSTRAINT refuse_all CHECK (false) NOT VALID"); err != nil {
		t.Fatalf("make audit inserts fail: %v", err)
	}
	h.expect(h.send(sent{method: "POST", path: "/api/v1/folders", bearer: h.adminToken, body: map[string]string{"name": "ghost"}}), http.StatusInternalServerError, "internal")
	h.expect(h.send(sent{method: "PUT", path: "/api/v1/admin/inventory-source", bearer: h.adminToken,
		body: map[string]string{"repo": invRepo, "branch": "main", "path": invPath}}), http.StatusInternalServerError, "internal")
	if _, err := h.pool.Exec(context.Background(), "ALTER TABLE audit_log DROP CONSTRAINT refuse_all"); err != nil {
		t.Fatalf("restore audit table: %v", err)
	}
	if list := h.mustDo("GET", "/api/v1/folders", nil, http.StatusOK); len(list["folders"].([]any)) != 0 {
		t.Errorf("folder exists although its audit entry failed: %v", list["folders"])
	}
	if _, ok, _ := store.GetInventorySource(context.Background(), h.pool); ok {
		t.Error("inventory source is stored although its audit entry failed")
	}
}

// A database failure while checking a token is the server's problem, not a bad credential.
func TestTokenCheckDatabaseFailureIs503Not401(t *testing.T) {
	h := newHarness(t)
	broken, err := db.Open(context.Background(), h.dbURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	broken.Close()
	h.rebuild(func(d *Deps) { d.Pool = broken })
	h.expect(h.send(sent{method: "GET", path: "/api/v1/me", bearer: h.adminToken}), http.StatusServiceUnavailable, "unavailable")
}

// Every registered route: without credentials 401; without the grant 403 (or
// allowed, for view); with the grant never 403. A route added without a
// decision, or wired to the wrong action, shows up here.
// adminOnlyRoutes need Admin on top of their action.
var adminOnlyRoutes = map[string]bool{
	"PUT /api/v1/admin/sso":                true,
	"POST /api/v1/admin/users/{id}/tokens": true,
}

// sessionOnlyRoutes answer 403 to any request made with a token.
var sessionOnlyRoutes = map[string]bool{
	"POST /api/v1/cli/login/confirm": true,
	"POST /api/v1/tokens":            true,
	"DELETE /api/v1/tokens/{id}":     true,
}

func TestEveryRouteEnforcesItsAction(t *testing.T) {
	h := newHarness(t)
	h.withSyncer(stubSyncer{})
	routes := (&server{}).routes()
	// Written from the task brief, independently of the code's own table: a
	// route given the wrong action, or added or dropped, makes this differ.
	expected := map[string]auth.Action{
		"GET /api/v1/me":                               auth.View,
		"POST /api/v1/auth/logout":                     auth.View,
		"GET /api/v1/inventory":                        auth.View,
		"GET /api/v1/inventory/services/{name}":        auth.View,
		"POST /api/v1/inventory/sync":                  auth.ManageConnections,
		"GET /api/v1/admin/inventory-source":           auth.ManageConnections,
		"PUT /api/v1/admin/inventory-source":           auth.ManageConnections,
		"GET /api/v1/admin/github-connections":         auth.ManageConnections,
		"POST /api/v1/admin/github-connections":        auth.ManageConnections,
		"GET /api/v1/admin/aws-accounts":               auth.ManageConnections,
		"POST /api/v1/admin/aws-accounts":              auth.ManageConnections,
		"POST /api/v1/admin/aws-accounts/{id}/test":    auth.ManageConnections,
		"GET /api/v1/admin/sso":                        auth.ManageAccess,
		"PUT /api/v1/admin/sso":                        auth.ManageAccess,
		"POST /api/v1/admin/sso/test":                  auth.ManageAccess,
		"GET /api/v1/folders":                          auth.View,
		"POST /api/v1/folders":                         auth.ManageJobs,
		"GET /api/v1/jobs":                             auth.View,
		"POST /api/v1/jobs":                            auth.ManageJobs,
		"GET /api/v1/jobs/{id}":                        auth.View,
		"PATCH /api/v1/jobs/{id}":                      auth.ManageJobs,
		"POST /api/v1/jobs/{id}/sync":                  auth.ManageJobs,
		"GET /api/v1/jobs/{id}/versions":               auth.View,
		"GET /api/v1/versions/running":                 auth.View,
		"GET /api/v1/admin/users":                      auth.ManageAccess,
		"POST /api/v1/admin/users":                     auth.ManageAccess,
		"POST /api/v1/admin/users/{id}/tokens":         auth.ManageAccess,
		"PATCH /api/v1/admin/users/{id}":               auth.ManageAccess,
		"GET /api/v1/admin/groups":                     auth.ManageAccess,
		"POST /api/v1/admin/groups":                    auth.ManageAccess,
		"PATCH /api/v1/admin/groups/{id}":              auth.ManageAccess,
		"GET /api/v1/admin/idp-mappings":               auth.ManageAccess,
		"GET /api/v1/admin/idp-mappings/{idpGroup}":    auth.ManageAccess,
		"PUT /api/v1/admin/idp-mappings/{idpGroup}":    auth.ManageAccess,
		"DELETE /api/v1/admin/idp-mappings/{idpGroup}": auth.ManageAccess,
		"GET /api/v1/admin/grants":                     auth.ManageAccess,
		"POST /api/v1/admin/grants":                    auth.ManageAccess,
		"DELETE /api/v1/admin/grants/{id}":             auth.ManageAccess,
		"GET /api/v1/admin/audit":                      auth.ViewAudit,
		"GET /api/v1/tokens":                           auth.View,
		"POST /api/v1/tokens":                          auth.View,
		"DELETE /api/v1/tokens/{id}":                   auth.View,
		"POST /api/v1/cli/login/confirm":               auth.View,
		// Run permissions depend on the run's target (a grant may be scoped),
		// so they are checked in the handlers; the route itself needs view.
		"POST /api/v1/jobs/{id}/runs":                   auth.View,
		"GET /api/v1/runs":                              auth.View,
		"GET /api/v1/runs/{id}":                         auth.View,
		"GET /api/v1/runs/{id}/log":                     auth.View,
		"GET /api/v1/runs/{id}/plan":                    auth.View,
		"GET /api/v1/runs/{id}/events":                  auth.View,
		"POST /api/v1/runs/{id}/start":                  auth.View,
		"POST /api/v1/runs/{id}/cancel":                 auth.View,
		"GET /api/v1/approvals":                         auth.View,
		"POST /api/v1/runs/{id}/approve":                auth.View,
		"POST /api/v1/runs/{id}/reject":                 auth.View,
		"POST /api/v1/runs/{id}/legs/{leg}/reconcile":   auth.View,
		"POST /api/v1/runs/{id}/legs/{leg}/check-again": auth.View,
		"POST /api/v1/admin/runs/{id}/release-locks":    auth.ManageAccess,
		"GET /api/v1/freezes":                           auth.View,
		"POST /api/v1/admin/freezes":                    auth.ManageAccess,
		"DELETE /api/v1/admin/freezes/{id}":             auth.ManageAccess,
		"GET /api/v1/admin/slack":                       auth.ManageConnections,
		"PUT /api/v1/admin/slack":                       auth.ManageConnections,
		"POST /api/v1/admin/slack/test":                 auth.ManageConnections,
		"GET /api/v1/analytics/overview":                auth.View,
		"GET /api/v1/queue":                             auth.View,
		"GET /api/v1/services/{name}":                   auth.View,
		"GET /api/v1/system/health":                     auth.ManageConnections,
	}
	if len(routes) != len(expected) {
		t.Fatalf("routes() has %d routes, the expected table %d", len(routes), len(expected))
	}
	for _, route := range routes {
		if want, ok := expected[route.pattern]; !ok || want != route.action {
			t.Errorf("route %q has action %q; the brief says %q (listed: %v)", route.pattern, route.action, want, ok)
		}
	}
	// An unlisted path is not served, even to an admin.
	h.expect(h.send(sent{method: "GET", path: "/api/v1/not-in-the-table", bearer: h.adminToken}), http.StatusNotFound, "not_found")
	plain := h.newUser("routeplain")
	plainToken, _ := h.newToken(plain.ID)
	granted := map[auth.Action]string{}
	for _, route := range routes {
		method, pattern, _ := strings.Cut(route.pattern, " ")
		path := strings.NewReplacer("{name}", "x", "{idpGroup}", "x", "{id}", "999999", "{leg}", "999999").Replace(pattern)
		name := route.pattern

		if rec := h.send(sent{method: method, path: path}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without credentials: %s, want 401", name, failure(rec))
		}
		bearer := plainToken
		if route.pattern == "POST /api/v1/auth/logout" {
			// Logging out with a token revokes it: use a throwaway one.
			bearer, _ = h.newToken(plain.ID)
		}
		rec := h.send(sent{method: method, path: path, bearer: bearer})
		if route.action == auth.View {
			// Confirming a device sign-in and minting or revoking a token need a
			// browser session: a token gets 403 by design.
			if rec.Code == http.StatusForbidden && !sessionOnlyRoutes[route.pattern] {
				t.Errorf("%s needs only view but a plain user got 403", name)
			}
			continue
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without a grant: %s, want 403", name, failure(rec))
		}
		token, ok := granted[route.action]
		if !ok {
			u := h.newUser("route" + string(route.action))
			h.grant(u.ID, string(route.action))
			token, _ = h.newToken(u.ID)
			granted[route.action] = token
		}
		// Changing the SSO settings needs Admin on top of manage_access.
		if rec := h.send(sent{method: method, path: path, bearer: token}); rec.Code == http.StatusForbidden && !adminOnlyRoutes[route.pattern] {
			t.Errorf("%s with %s granted still got 403", name, route.action)
		}
	}
}

// A pattern registered next to the table, without a permission decision, must
// never reach its handler.
func TestRouteOutsideTheTableIsNeverServed(t *testing.T) {
	h := newHarness(t)
	reached := false
	deps := h.deps
	deps.Now = time.Now
	s := &server{Deps: deps}
	handler := s.authenticate(s.authenticatedMux(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /api/v1/sneaky", func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		})
	}))
	req := httptest.NewRequest("GET", "/api/v1/sneaky", nil)
	req.Header.Set("Authorization", "Bearer "+h.adminToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if reached || rec.Code != http.StatusNotFound {
		t.Fatalf("a route outside the table: reached=%v, %s; want it refused with 404", reached, failure(rec))
	}
}
