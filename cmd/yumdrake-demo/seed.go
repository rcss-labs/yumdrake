package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/rcss-labs/yamdrake/internal/adminuser"
	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// The demo people. The passwords are PUBLIC by design (they are printed in the README): the demo
// refuses to run outside localhost (see internal/config), so they protect nothing real.
const (
	adminEmail    = "admin@demo.yumdrake.test"
	adminName     = "Demo Admin"
	adminPassword = "demo-admin-password" //nolint:gosec // public demo password

	deployerEmail    = "deployer@demo.yumdrake.test"
	deployerName     = "Demo Deployer"
	deployerPassword = "demo-deployer-password" //nolint:gosec // public demo password

	viewerEmail    = "viewer@demo.yumdrake.test"
	viewerName     = "Demo Viewer"
	viewerPassword = "demo-viewer-password" //nolint:gosec // public demo password

	deployersGroup = "Deployers"
	viewersGroup   = "Viewers"

	sessionCookie = "yumdrake_session"
	waitLimit     = 3 * time.Minute
)

type seedConfig struct {
	// OrchestratorURL is where the orchestrator answers inside the Compose network.
	OrchestratorURL string
	// PublicURL is the address people open; it is sent as the Origin of every write.
	PublicURL   string
	DatabaseURL string
	Log         *slog.Logger
}

type demoUser struct {
	email, name, password, group string
}

// demoFolders maps a folder to the job files in it.
var demoFolders = []struct {
	name string
	jobs []string
}{
	{"Backend", []string{"jobs/deploy-backend-api.yaml", "jobs/deploy-platform.yaml"}},
	{"Frontend", []string{"jobs/deploy-web.yaml"}},
}

var demoPeople = []demoUser{
	{deployerEmail, deployerName, deployerPassword, deployersGroup},
	{viewerEmail, viewerName, viewerPassword, viewersGroup},
}

type demoGrant struct{ group, action, environment string }

// The deployer may run and cancel in dev and qa only, and read logs everywhere; the viewer only reads.
var demoGrants = []demoGrant{
	{deployersGroup, "run", "dev"},
	{deployersGroup, "run", "qa"},
	{deployersGroup, "cancel", "dev"},
	{deployersGroup, "cancel", "qa"},
	{deployersGroup, "view_logs", ""},
	{viewersGroup, "view_logs", ""},
}

// seed brings a fresh Yumdrake to the demo state and does nothing to one already there. It reads
// what exists straight from the database and writes through the API (so every change is validated
// and audited as a person's would be). It signs in only when something is missing: a sign-in is
// itself audited, and a repeat run must leave no trace.
func seed(ctx context.Context, cfg seedConfig) error {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if err := waitFor(ctx, "the orchestrator to answer /readyz", func() (bool, error) {
		return get(ctx, cfg.OrchestratorURL+"/readyz") == http.StatusOK, nil
	}); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer pool.Close()

	s := &seeder{cfg: cfg, pool: pool, api: &apiClient{base: cfg.OrchestratorURL, origin: cfg.PublicURL}}
	steps := []struct {
		what string
		run  func(context.Context) error
	}{
		{"the admin user", s.ensureAdmin},
		{"the GitHub connection", s.ensureConnection},
		{"the inventory source", s.ensureInventorySource},
		{"the folders", s.ensureFolders},
		{"the jobs", s.ensureJobs},
		{"the groups", s.ensureGroups},
		{"the people", s.ensurePeople},
		{"the grants", s.ensureGrants},
		{"the jobs to be runnable and the inventory active", s.waitRunnable},
	}
	for _, step := range steps {
		if err := step.run(ctx); err != nil {
			return fmt.Errorf("%s: %w", step.what, err)
		}
	}
	return nil
}

type seeder struct {
	cfg  seedConfig
	pool store.DBTX
	api  *apiClient
}

func (s *seeder) ensureAdmin(ctx context.Context) error {
	if _, err := store.UserByEmail(ctx, s.pool, adminEmail); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	hash, err := auth.HashPassword(adminPassword)
	if err != nil {
		return err
	}
	return createAdminIn(ctx, s.cfg.DatabaseURL, hash)
}

func (s *seeder) ensureConnection(ctx context.Context) error {
	connections, err := store.ListConnections(ctx, s.pool)
	if err != nil {
		return err
	}
	for _, c := range connections {
		if c.Owner == demoOwner {
			return nil
		}
	}
	return s.api.call(ctx, http.MethodPost, "/api/v1/admin/github-connections",
		map[string]any{"owner": demoOwner, "method": "token", "token": demoGitHubToken}, nil)
}

func (s *seeder) ensureInventorySource(ctx context.Context) error {
	source, _, err := store.GetInventorySource(ctx, s.pool)
	if err != nil {
		return err
	}
	if source.Repo == demoRepo && source.Branch == demoBranch && source.Path == "" {
		return nil
	}
	return s.api.call(ctx, http.MethodPut, "/api/v1/admin/inventory-source",
		map[string]any{"repo": demoRepo, "branch": demoBranch}, nil)
}

func (s *seeder) folderIDs(ctx context.Context) (map[string]int64, error) {
	folders, err := store.ListFolders(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]int64, len(folders))
	for _, f := range folders {
		ids[f.Name] = f.ID
	}
	return ids, nil
}

func (s *seeder) ensureFolders(ctx context.Context) error {
	ids, err := s.folderIDs(ctx)
	if err != nil {
		return err
	}
	for _, folder := range demoFolders {
		if _, ok := ids[folder.name]; ok {
			continue
		}
		if err := s.api.call(ctx, http.MethodPost, "/api/v1/folders", map[string]any{"name": folder.name}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *seeder) ensureJobs(ctx context.Context) error {
	ids, err := s.folderIDs(ctx)
	if err != nil {
		return err
	}
	jobs, err := store.ListJobSources(ctx, s.pool)
	if err != nil {
		return err
	}
	registered := map[string]bool{}
	for _, j := range jobs {
		registered[j.Repo+"@"+j.Branch+":"+j.Path] = true
	}
	for _, folder := range demoFolders {
		for _, path := range folder.jobs {
			if registered[demoRepo+"@"+demoBranch+":"+path] {
				continue
			}
			if err := s.api.call(ctx, http.MethodPost, "/api/v1/jobs", map[string]any{
				"folder_id": ids[folder.name], "repo": demoRepo, "branch": demoBranch, "path": path}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *seeder) groupIDs(ctx context.Context) (map[string]int64, error) {
	groups, err := store.ListGroups(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]int64, len(groups))
	for _, g := range groups {
		ids[g.Name] = g.ID
	}
	return ids, nil
}

func (s *seeder) ensureGroups(ctx context.Context) error {
	ids, err := s.groupIDs(ctx)
	if err != nil {
		return err
	}
	for _, name := range []string{deployersGroup, viewersGroup} {
		if _, ok := ids[name]; ok {
			continue
		}
		if err := s.api.call(ctx, http.MethodPost, "/api/v1/admin/groups", map[string]any{"name": name}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *seeder) ensurePeople(ctx context.Context) error {
	groups, err := s.groupIDs(ctx)
	if err != nil {
		return err
	}
	for _, person := range demoPeople {
		user, err := store.UserByEmail(ctx, s.pool, person.email)
		if errors.Is(err, store.ErrNotFound) {
			var created struct {
				ID int64 `json:"id"`
			}
			if err := s.api.call(ctx, http.MethodPost, "/api/v1/admin/users", map[string]any{
				"email": person.email, "name": person.name, "source": "local", "password": person.password}, &created); err != nil {
				return err
			}
			user.ID = created.ID
		} else if err != nil {
			return err
		}
		memberOf, err := store.ManualGroupIDs(ctx, s.pool, user.ID)
		if err != nil {
			return err
		}
		if slices.Contains(memberOf, groups[person.group]) {
			continue
		}
		if err := s.api.call(ctx, http.MethodPatch, fmt.Sprintf("/api/v1/admin/users/%d", user.ID),
			map[string]any{"group_ids": append(memberOf, groups[person.group])}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *seeder) ensureGrants(ctx context.Context) error {
	groups, err := s.groupIDs(ctx)
	if err != nil {
		return err
	}
	existing, err := store.ListGrants(ctx, s.pool)
	if err != nil {
		return err
	}
	for _, want := range demoGrants {
		groupID := groups[want.group]
		if slices.ContainsFunc(existing, func(g store.Grant) bool {
			return g.GroupID == groupID && g.Action == want.action && g.Environment == want.environment && g.Label == "" && g.FolderID == 0
		}) {
			continue
		}
		if err := s.api.call(ctx, http.MethodPost, "/api/v1/admin/grants", map[string]any{
			"group_id": groupID, "action": want.action, "environment": want.environment}, nil); err != nil {
			return err
		}
	}
	return nil
}

// waitRunnable waits for the sync to finish: the inventory of main is active and every demo job
// has a good definition.
func (s *seeder) waitRunnable(ctx context.Context) error {
	want := 0
	for _, folder := range demoFolders {
		want += len(folder.jobs)
	}
	return waitFor(ctx, "the jobs to be runnable and the inventory active", func() (bool, error) {
		snapshot, active, err := store.ActiveSnapshot(ctx, s.pool)
		if err != nil || !active || snapshot.Commit != demoMainCommit {
			return false, err
		}
		jobs, err := store.ListJobSources(ctx, s.pool)
		if err != nil {
			return false, err
		}
		runnable := 0
		for _, j := range jobs {
			if j.Runnable {
				runnable++
			}
		}
		return runnable >= want, nil
	})
}

// createAdminIn makes the admin through the code `yumdrake-orchestrator create-admin` uses.
func createAdminIn(ctx context.Context, databaseURL, hash string) error {
	pool, err := db.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := adminuser.Create(ctx, tx, adminEmail, adminName, hash, time.Now(), "demo"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func waitFor(ctx context.Context, what string, cond func() (bool, error)) error {
	deadline := time.Now().Add(waitLimit)
	for {
		ok, err := cond()
		if err != nil {
			return fmt.Errorf("waiting for %s: %w", what, err)
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func get(ctx context.Context, url string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// apiClient calls the orchestrator as the demo admin. It signs in on the first call only.
type apiClient struct {
	base, origin string
	session      string
}

func (c *apiClient) call(ctx context.Context, method, path string, body, out any) error {
	if c.session == "" {
		if err := c.login(ctx); err != nil {
			return err
		}
	}
	resp, err := c.do(ctx, method, path, body, c.session)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		return nil
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

func (c *apiClient) login(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodPost, "/api/v1/auth/login",
		map[string]string{"email": adminEmail, "password": adminPassword}, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	for _, cookie := range resp.Cookies() {
		if cookie.Name == sessionCookie {
			c.session = cookie.Value
			return nil
		}
	}
	return errors.New("sign-in set no session cookie")
}

// do sends one request. A failure reports the status and the error code only, never a body.
func (c *apiClient) do(ctx context.Context, method, path string, body any, session string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if session != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session}) //nolint:gosec // a request cookie
	}
	req.Header.Set("Origin", c.origin)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &failure)
		return nil, fmt.Errorf("%s %s: status %d, error code %q", method, path, resp.StatusCode, failure.Error.Code)
	}
	return resp, nil
}
