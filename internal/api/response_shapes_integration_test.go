//go:build integration

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/rcss-labs/yamdrake/internal/running"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/versions"
)

// nullSlices returns the JSON paths in raw where the Go type t declares a slice
// but the document has null. The web application iterates every list, so null
// is a bug. json.RawMessage and []byte are not lists.
func nullSlices(t reflect.Type, raw json.RawMessage, path string) []string {
	isNull := strings.TrimSpace(string(raw)) == "null"
	for t.Kind() == reflect.Pointer {
		if isNull {
			return nil
		}
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		if isNull {
			return []string{path}
		}
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return nil
		}
		var found []string
		for i, item := range items {
			found = append(found, nullSlices(t.Elem(), item, path+"["+itoa(int64(i))+"]")...)
		}
		return found
	case reflect.Map:
		var items map[string]json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return nil
		}
		var found []string
		for key, item := range items {
			found = append(found, nullSlices(t.Elem(), item, path+"."+key)...)
		}
		return found
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return nil
		}
		return structNullSlices(t, fields, path)
	}
	return nil
}

func structNullSlices(t reflect.Type, fields map[string]json.RawMessage, path string) []string {
	var found []string
	for i := range t.NumField() {
		field := t.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if field.Anonymous && name == "" {
			found = append(found, structNullSlices(field.Type, fields, path)...)
			continue
		}
		if name == "-" || !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		if value, ok := fields[name]; ok {
			found = append(found, nullSlices(field.Type, value, path+"."+name)...)
		}
	}
	return found
}

// listRoute is a GET route whose answer holds lists, with the Go type that
// declares them.
type listRoute struct {
	pattern string // as registered in routes()
	path    string
	into    reflect.Type
}

// notAListResponse names the GET routes that answer a single object with no
// list inside; every other GET route must be in the table below, so a new one
// cannot be forgotten.
var notAListResponse = map[string]bool{
	"GET /api/v1/admin/idp-mappings/{idpGroup}": true,
	// ssoSettingsView holds only strings and a bool: no slice fields.
	"GET /api/v1/admin/sso": true,
	// sourceRequest holds only three strings.
	"GET /api/v1/admin/inventory-source": true,
	// slackResponse holds a bool and two nullable scalars: no slice fields.
	"GET /api/v1/admin/slack": true,
	// overviewResponse is checked for [] with no runs by TestOverviewWithNoRunsHasEmptyListsAndNullFigures.
	"GET /api/v1/analytics/overview": true,
	// The run routes need runs, so TestGetRunIncludesFailureSummary,
	// TestListRunsFilters and TestLogEndpointFiltersPagesAndNeedsViewLogs check
	// their lists with nullSlices; the event stream is not JSON.
	"GET /api/v1/runs":          true,
	"GET /api/v1/runs/{id}":     true,
	"GET /api/v1/runs/{id}/log": true,
	// The plan needs a stored manifest; TestPlanEndpointServesRedactedManifest
	// checks its lists with nullSlices.
	"GET /api/v1/runs/{id}/plan":   true,
	"GET /api/v1/runs/{id}/events": true,
}

// Every list a route returns is [] when empty, never null, on an empty
// database and on a populated one.
func TestNoRouteReturnsNullForAList(t *testing.T) {
	h := newHarness(t)
	list := func(pattern, path string, into reflect.Type) listRoute { return listRoute{pattern, path, into} }
	empty := []listRoute{
		list("GET /api/v1/me", "/api/v1/me", reflect.TypeFor[principalResponse]()),
		list("GET /api/v1/inventory", "/api/v1/inventory", reflect.TypeFor[inventoryResponse]()),
		list("GET /api/v1/folders", "/api/v1/folders", reflect.TypeFor[struct {
			Folders []folderResponse `json:"folders"`
		}]()),
		list("GET /api/v1/jobs", "/api/v1/jobs", reflect.TypeFor[struct {
			Jobs []jobSummary `json:"jobs"`
		}]()),
		list("GET /api/v1/admin/github-connections", "/api/v1/admin/github-connections", reflect.TypeFor[struct {
			Connections []connectionResponse `json:"connections"`
		}]()),
		list("GET /api/v1/admin/aws-accounts", "/api/v1/admin/aws-accounts", reflect.TypeFor[struct {
			Accounts []awsAccountResponse `json:"accounts"`
		}]()),
		list("GET /api/v1/admin/users", "/api/v1/admin/users", reflect.TypeFor[struct {
			Users []userResponse `json:"users"`
		}]()),
		list("GET /api/v1/admin/groups", "/api/v1/admin/groups", reflect.TypeFor[struct {
			Groups []groupView `json:"groups"`
		}]()),
		list("GET /api/v1/admin/idp-mappings", "/api/v1/admin/idp-mappings", reflect.TypeFor[struct {
			Mappings []mappingView `json:"mappings"`
		}]()),
		list("GET /api/v1/admin/grants", "/api/v1/admin/grants", reflect.TypeFor[struct {
			Grants []grantResponse `json:"grants"`
		}]()),
		list("GET /api/v1/admin/audit", "/api/v1/admin/audit", reflect.TypeFor[struct {
			Entries    []auditEntryResponse `json:"entries"`
			NextBefore *int64               `json:"next_before"`
		}]()),
		list("GET /api/v1/tokens", "/api/v1/tokens", reflect.TypeFor[struct {
			Tokens []tokenResponse `json:"tokens"`
		}]()),
		list("GET /api/v1/freezes", "/api/v1/freezes", reflect.TypeFor[freezeListResponse]()),
		list("GET /api/v1/approvals", "/api/v1/approvals", reflect.TypeFor[approvalListResponse]()),
		list("GET /api/v1/queue", "/api/v1/queue", reflect.TypeFor[queueResponse]()),
		list("GET /api/v1/system/health", "/api/v1/system/health", reflect.TypeFor[healthResponse]()),
	}
	check := func(stage string, routes []listRoute) {
		t.Helper()
		for _, r := range routes {
			code, raw := h.doRaw("GET", r.path, h.adminToken, nil)
			if code != http.StatusOK {
				t.Fatalf("%s: GET %s = %d", stage, r.path, code)
			}
			if found := nullSlices(r.into, raw, ""); len(found) > 0 {
				t.Errorf("%s: GET %s has null where a list belongs: %v", stage, r.path, found)
			}
		}
	}
	check("empty database", empty)

	// No GET route may be missing from the table (or the allowlist).
	listed := map[string]bool{}
	for _, r := range empty {
		listed[r.pattern] = true
	}
	populatedOnly := []listRoute{
		list("GET /api/v1/inventory/services/{name}", "/api/v1/inventory/services/orders", reflect.TypeFor[serviceDetail]()),
		list("GET /api/v1/inventory/services/{name}", "/api/v1/inventory/services/backend-api", reflect.TypeFor[serviceDetail]()),
		list("GET /api/v1/services/{name}", "/api/v1/services/orders", reflect.TypeFor[servicePageResponse]()),
	}
	for _, r := range populatedOnly {
		listed[r.pattern] = true
	}
	listed["GET /api/v1/jobs/{id}"] = true // checked below with the id of the registered job
	listed["GET /api/v1/jobs/{id}/versions"] = true
	listed["GET /api/v1/versions/running"] = true // 409 on an empty database; checked populated below
	for _, route := range (&server{}).routes() {
		if strings.HasPrefix(route.pattern, "GET ") && !listed[route.pattern] && !notAListResponse[route.pattern] {
			t.Errorf("%s is neither in the list-shape table nor in notAListResponse", route.pattern)
		}
	}

	ctx := context.Background()
	h.pushInventory("c1")
	h.pushJob("j1")
	h.setInventorySource()
	folder := h.createFolder("Backends")
	created := h.registerJob(folder)
	jobID := itoa(int64(created["id"].(float64)))
	h.mustDo("POST", "/api/v1/admin/github-connections", map[string]any{"owner": "acme", "method": "token", "token": "ghp_x"}, http.StatusCreated)
	h.mustDo("POST", "/api/v1/admin/aws-accounts", awsRoleBody("empty-env", "999999999999"), http.StatusCreated)
	h.newUser("nogroups") // a user who belongs to no group
	group := h.createGroupVia("Operators", false)
	if err := store.SetIDPMapping(ctx, h.pool, "idp-operators", group); err != nil {
		t.Fatalf("SetIDPMapping: %v", err)
	}
	if _, err := store.CreateGrant(ctx, h.pool, store.Grant{GroupID: group, Action: "run"}); err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	populated := append(append([]listRoute{}, empty...), populatedOnly...)
	populated = append(populated, list("GET /api/v1/jobs/{id}", "/api/v1/jobs/"+jobID, reflect.TypeFor[jobDetail]()))
	// No AWS account is connected here, so every service is "unavailable":
	// the empty and unavailable shapes must still be lists and maps, never null.
	populated = append(populated, list("GET /api/v1/jobs/{id}/versions", "/api/v1/jobs/"+jobID+"/versions?environment=prod-01", reflect.TypeFor[versions.JobVersions]()))
	populated = append(populated, list("GET /api/v1/versions/running", "/api/v1/versions/running", reflect.TypeFor[running.Matrix]()))
	check("populated database", populated)

	// The stored job definition is raw JSON in the response type; walk it as a spec.Job.
	_, raw := h.doRaw("GET", "/api/v1/jobs/"+jobID, h.adminToken, nil)
	var detail struct {
		Definition json.RawMessage `json:"definition"`
	}
	_ = json.Unmarshal(raw, &detail)
	if found := nullSlices(reflect.TypeFor[spec.Job](), detail.Definition, ".definition"); len(found) > 0 {
		t.Errorf("job definition has null where a list belongs: %v", found)
	}

	// A rejected commit whose stored problems document is JSON null.
	if _, err := h.pool.Exec(ctx, "UPDATE inventory_status SET problems = 'null'::jsonb, problems_commit = 'deadbeef'"); err != nil {
		t.Fatalf("store a null problems document: %v", err)
	}
	check("rejected commit with null problems", empty[1:2])
}

// /me and the sign-in answer carry exactly the fields the web application reads.
func TestPrincipalResponsesHaveTheWebContractShape(t *testing.T) {
	h := newHarness(t)
	user := h.newUser("shape")
	h.grant(user.ID, "manage_jobs")
	keys := func(m map[string]any) string {
		var names []string
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		return strings.Join(names, ",")
	}
	check := func(what string, raw []byte) {
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("%s: decode: %v", what, err)
		}
		if got := keys(body); got != "admin,email,grants,id,name" {
			t.Errorf("%s fields = %s", what, got)
		}
		grants, _ := body["grants"].([]any)
		if len(grants) != 1 {
			t.Fatalf("%s grants = %v, want one", what, body["grants"])
		}
		if got := keys(grants[0].(map[string]any)); got != "action,environment,folder_id,label" {
			t.Errorf("%s grant fields = %s", what, got)
		}
	}
	rec := h.login(user.Email, testPassword)
	h.expect(rec, http.StatusOK, "")
	check("login", rec.Body.Bytes())
	// A user with no grants gets [] and not null.
	bare := h.newUser("nogrants")
	bareToken, _ := h.newToken(bare.ID)
	me0 := h.send(sent{method: "GET", path: "/api/v1/me", bearer: bareToken})
	h.expect(me0, http.StatusOK, "")
	if !strings.Contains(me0.Body.String(), `"grants":[]`) {
		t.Errorf("/me of a user without grants: grants is not []")
	}
	token, _ := h.newToken(user.ID)
	me := h.send(sent{method: "GET", path: "/api/v1/me", bearer: token})
	h.expect(me, http.StatusOK, "")
	check("/me", me.Body.Bytes())
}
