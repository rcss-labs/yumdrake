//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/cache"
	"github.com/rcss-labs/yamdrake/internal/db"
	runengine "github.com/rcss-labs/yamdrake/internal/engine"
	"github.com/rcss-labs/yamdrake/internal/logs"
	"github.com/rcss-labs/yamdrake/internal/secrets"
	"github.com/rcss-labs/yamdrake/internal/session"
	"github.com/rcss-labs/yamdrake/internal/store"
	yumsync "github.com/rcss-labs/yamdrake/internal/sync"
	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
)

const (
	// publicURL is what the harness's API believes its public address is.
	publicURL = "https://yumdrake.test"
	invRepo   = "acme/inventory"
	invPath   = "inv"
	jobsRepo  = "acme/jobs"
	jobPath   = "jobs/deploy-backend.yaml"
)

// fakeSource serves files from memory keyed "repo@commit/path".
type fakeSource struct {
	mu    gosync.Mutex
	files map[string][]byte
	heads map[string]string
	err   error
}

func (f *fakeSource) put(repo, commit, path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[repo+"@"+commit+"/"+path] = data
}

func (f *fakeSource) setHead(repo, branch, commit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heads[repo+"@"+branch] = commit
}

func (f *fakeSource) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeSource) HeadCommit(_ context.Context, repo, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	return f.heads[repo+"@"+branch], nil
}

func (f *fakeSource) ListFiles(_ context.Context, repo, commit, dir string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := repo + "@" + commit + "/"
	var out []string
	for key := range f.files {
		if p, ok := strings.CutPrefix(key, prefix); ok && strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeSource) ReadFile(_ context.Context, repo, commit, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[repo+"@"+commit+"/"+path]
	if !ok {
		return nil, fmt.Errorf("fake: %s@%s/%s: not found", repo, commit, path)
	}
	return data, nil
}

type harness struct {
	t       *testing.T
	pool    *pgxpool.Pool
	cipher  secrets.Cipher
	src     *fakeSource
	handler http.Handler
	bodies  []string // every response body, for the secret checks
	// bodiesMu guards bodies: some tests send requests from several goroutines.
	bodiesMu gosync.Mutex
	// logs holds everything the server logged, for the secret checks.
	logs *lockedBuffer

	dbURL      string
	deps       Deps
	valkey     valkey.Client
	sessions   *session.Manager
	admin      store.User // a member of an admin group
	adminToken string
}

func valkeyAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("YUMDRAKE_TEST_VALKEY_ADDR")
	if addr == "" {
		t.Fatal("YUMDRAKE_TEST_VALKEY_ADDR must be set for integration tests")
	}
	return addr
}

func openValkey(t *testing.T, addr string) valkey.Client {
	t.Helper()
	c, err := cache.Open(cache.Options{Addr: addr})
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// uniqueEmail keeps tests apart in the Valkey instance they share.
func uniqueEmail(t *testing.T, label string) string {
	t.Helper()
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random: %v", err)
	}
	return fmt.Sprintf("%s-%x@example.test", label, raw)
}

// testLockoutWindow is long enough that five sign-ins in a row (each costs an
// argon2 check, slow under -race) fit in it, and short enough to wait out.
const testLockoutWindow = 5 * time.Second

// testPassword is a valid password for users made by the harness.
const testPassword = "correct horse battery staple"

// newUser creates a local user with testPassword.
func (h *harness) newUser(label string) store.User {
	h.t.Helper()
	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		h.t.Fatalf("HashPassword: %v", err)
	}
	u, err := store.CreateLocalUser(context.Background(), h.pool, uniqueEmail(h.t, label), label, "local", hash)
	if err != nil {
		h.t.Fatalf("CreateLocalUser: %v", err)
	}
	return u
}

func (h *harness) newToken(userID int64) (plaintext string, tok store.Token) {
	h.t.Helper()
	plaintext, tok, err := auth.NewToken(context.Background(), h.pool, userID, "test", time.Hour, time.Now())
	if err != nil {
		h.t.Fatalf("NewToken: %v", err)
	}
	return plaintext, tok
}

func (h *harness) newSession(userID int64) string {
	h.t.Helper()
	s, err := h.sessions.Create(context.Background(), userID)
	if err != nil {
		h.t.Fatalf("session Create: %v", err)
	}
	return s.ID
}

// grant gives userID a new group holding the action.
func (h *harness) grant(userID int64, action string) {
	h.t.Helper()
	ctx := context.Background()
	group, err := store.CreateGroup(ctx, h.pool, "g-"+uniqueEmail(h.t, action), false)
	if err != nil {
		h.t.Fatalf("CreateGroup: %v", err)
	}
	if err := store.SetManualMembership(ctx, h.pool, userID, group.ID, true); err != nil {
		h.t.Fatalf("SetManualMembership: %v", err)
	}
	if _, err := store.CreateGrant(ctx, h.pool, store.Grant{GroupID: group.ID, Action: action}); err != nil {
		h.t.Fatalf("CreateGrant: %v", err)
	}
}

// rebuild replaces the handler with one built from the harness's deps after
// change has adjusted them.
func (h *harness) rebuild(change func(*Deps)) {
	h.t.Helper()
	deps := h.deps
	change(&deps)
	h.handler = New(deps)
}

func (h *harness) auditEntries() []store.AuditEntry {
	h.t.Helper()
	entries, err := store.ListAudit(context.Background(), h.pool, 0, 200)
	if err != nil {
		h.t.Fatalf("ListAudit: %v", err)
	}
	return entries
}

// auditActions lists the actions of the audit entries, oldest first.
func (h *harness) auditActions() []string {
	entries := h.auditEntries()
	out := make([]string, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		out = append(out, entries[i].Action)
	}
	return out
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	url := testdb.New(t)
	if err := db.Migrate(ctx, url); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	cipher, err := secrets.NewLocal([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	src := &fakeSource{files: map[string][]byte{}, heads: map[string]string{}}
	logBuffer := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(logBuffer, nil))
	engine := yumsync.NewEngine(pool,
		func(context.Context, string) (yumsync.Source, error) { return src, nil },
		log, time.Now)
	client := openValkey(t, valkeyAddr(t))
	prefix := fmt.Sprintf("test-%x:", randomBytes(t, 8)) // runs sharing the Valkey must not see each other
	sessions := session.NewManager(client, time.Now, session.WithKeyPrefix(prefix))
	deps := Deps{
		Pool: pool, Engine: engine, Cipher: cipher, Log: log,
		Sessions: sessions, Valkey: client, KeyPrefix: prefix, PublicURL: publicURL,
		LockoutWindow: testLockoutWindow,
		Runs:          runengine.New(runengine.Deps{DB: pool, Log: log}),
		Logs:          logs.NewReader(pool, nil),
	}
	h := &harness{t: t, logs: logBuffer, dbURL: url, pool: pool, cipher: cipher, src: src, handler: New(deps),
		deps: deps, valkey: client, sessions: sessions}
	h.admin = h.newUser("admin")
	adminGroup, err := store.CreateGroup(ctx, pool, "Admins", true)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if err := store.SetManualMembership(ctx, pool, h.admin.ID, adminGroup.ID, true); err != nil {
		t.Fatalf("SetManualMembership: %v", err)
	}
	h.adminToken, _ = h.newToken(h.admin.ID)
	return h
}

func fixture(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "spec", "testdata", "valid", rel))
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	return data
}

func (h *harness) pushInventory(commit string) {
	h.src.put(invRepo, commit, invPath+"/environments.yaml", fixture(h.t, "environments.yaml"))
	h.src.put(invRepo, commit, invPath+"/services/orders.yaml", fixture(h.t, "services/orders.yaml"))
	h.src.put(invRepo, commit, invPath+"/services/backend-api.yaml", fixture(h.t, "services/backend-api.yaml"))
	h.src.setHead(invRepo, "main", commit)
}

func (h *harness) pushJob(commit string) {
	h.src.put(jobsRepo, commit, jobPath, fixture(h.t, "jobs/deploy-backend.yaml"))
	h.src.setHead(jobsRepo, "main", commit)
}

// do sends an authorised JSON request and records the body.
func (h *harness) do(method, path string, body any) (int, map[string]any) {
	h.t.Helper()
	code, raw := h.doRaw(method, path, h.adminToken, body)
	var decoded map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return code, decoded
}

func (h *harness) doRaw(method, path, token string, body any) (int, []byte) {
	h.t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			h.t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	h.bodies = append(h.bodies, rec.Body.String())
	return rec.Code, rec.Body.Bytes()
}

func (h *harness) mustDo(method, path string, body any, want int) map[string]any {
	h.t.Helper()
	code, got := h.do(method, path, body)
	if code != want {
		h.t.Fatalf("%s %s = %d, want %d (body %v)", method, path, code, want, got)
	}
	return got
}

func errorCode(t *testing.T, body map[string]any) string {
	t.Helper()
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func errorMessage(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

func (h *harness) setInventorySource() {
	h.t.Helper()
	h.mustDo("PUT", "/api/v1/admin/inventory-source", map[string]string{"repo": invRepo, "branch": "main", "path": invPath}, http.StatusOK)
}

func (h *harness) createFolder(name string) int64 {
	h.t.Helper()
	body := h.mustDo("POST", "/api/v1/folders", map[string]string{"name": name}, http.StatusCreated)
	return int64(body["id"].(float64))
}

func (h *harness) registerJob(folderID int64) map[string]any {
	h.t.Helper()
	return h.mustDo("POST", "/api/v1/jobs", map[string]any{
		"folder_id": folderID, "repo": jobsRepo, "branch": "main", "path": jobPath,
	}, http.StatusCreated)
}

func TestUnknownPathWithoutTokenIsUnauthorized(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.doRaw("GET", "/api/v1/nope", "", nil); code != http.StatusUnauthorized {
		t.Errorf("unknown path without token = %d, want 401", code)
	}
}

func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestCreateConnectionEncryptsAndNeverReturnsSecret(t *testing.T) {
	h := newHarness(t)
	const token = "ghp_super-secret-token-value-123"
	created := h.mustDo("POST", "/api/v1/admin/github-connections",
		map[string]any{"owner": "Acme", "method": "token", "token": token}, http.StatusCreated)
	if created["owner"] != "acme" {
		t.Errorf("owner = %v, want it normalised to lower case", created["owner"])
	}
	h.mustDo("GET", "/api/v1/admin/github-connections", nil, http.StatusOK)
	// A duplicate (different case) and a rejected request must not echo it either.
	h.mustDo("POST", "/api/v1/admin/github-connections",
		map[string]any{"owner": "ACME", "method": "token", "token": token}, http.StatusConflict)
	h.mustDo("POST", "/api/v1/admin/github-connections",
		map[string]any{"owner": "other", "method": "token", "token": token, "bogus": 1}, http.StatusBadRequest)

	// An App connection: neither the key nor any line of it may come back.
	pemKey := testPrivateKeyPEM(t)
	h.mustDo("POST", "/api/v1/admin/github-connections",
		map[string]any{"owner": "appowner", "method": "app", "app_id": 7, "installation_id": 9, "private_key": pemKey}, http.StatusCreated)
	h.mustDo("GET", "/api/v1/admin/github-connections", nil, http.StatusOK)

	ctx := context.Background()
	conn, ciphertext, err := store.ConnectionSecret(ctx, h.pool, "acme")
	if err != nil {
		t.Fatalf("ConnectionSecret: %v", err)
	}
	if conn.Method != "token" {
		t.Errorf("method = %q, want token", conn.Method)
	}
	if bytes.Contains(ciphertext, []byte(token)) {
		t.Fatal("stored secret contains the plaintext token")
	}
	plain, err := h.cipher.Decrypt(ctx, "github-connection", ciphertext)
	if err != nil || string(plain) != token {
		t.Fatalf("Decrypt with purpose github-connection = %q, %v; want the token", plain, err)
	}
	if _, err := h.cipher.Decrypt(ctx, "other-purpose", ciphertext); err == nil {
		t.Error("ciphertext decrypts under a different purpose")
	}
	for i, body := range h.bodies {
		if strings.Contains(body, token) {
			t.Errorf("response %d contains the token: %s", i, body)
		}
	}
	var pemLines []string
	for _, line := range strings.Split(pemKey, "\n") {
		if len(line) >= 20 && !strings.HasPrefix(line, "-----") {
			pemLines = append(pemLines, line)
		}
	}
	if len(pemLines) == 0 {
		t.Fatal("test key has no body lines")
	}
	for i, body := range h.bodies {
		if strings.Contains(body, "PRIVATE KEY") {
			t.Errorf("response %d contains PEM markers: %s", i, body)
		}
		for _, line := range pemLines {
			if strings.Contains(body, line) {
				t.Errorf("response %d contains a line of the private key", i)
			}
		}
	}
	_, appCiphertext, err := store.ConnectionSecret(ctx, h.pool, "appowner")
	if err != nil {
		t.Fatalf("ConnectionSecret(appowner): %v", err)
	}
	if plain, err := h.cipher.Decrypt(ctx, "github-connection", appCiphertext); err != nil || string(plain) != pemKey {
		t.Errorf("stored App key does not decrypt to the submitted PEM (err %v)", err)
	}
	if len(h.bodies) < 4 {
		t.Fatalf("recorded %d responses, want at least 4", len(h.bodies))
	}
}

func TestCreateConnectionValidatesInput(t *testing.T) {
	h := newHarness(t)
	for name, body := range map[string]map[string]any{
		"token method without token": {"owner": "acme", "method": "token"},
		"app method without key":     {"owner": "acme", "method": "app", "app_id": 1, "installation_id": 2},
		"app method without ids":     {"owner": "acme", "method": "app", "private_key": "x"},
		"unknown method":             {"owner": "acme", "method": "password", "token": "t"},
		"missing owner":              {"method": "token", "token": "t"},
		"owner with slash":           {"owner": "acme/evil", "method": "token", "token": "t"},
		"app with malformed key":     {"owner": "acme", "method": "app", "app_id": 1, "installation_id": 2, "private_key": "not a pem"},
	} {
		code, got := h.do("POST", "/api/v1/admin/github-connections", body)
		if code != http.StatusBadRequest || errorCode(t, got) != "invalid" {
			t.Errorf("%s: status %d code %q, want 400 invalid", name, code, errorCode(t, got))
		}
	}
}

func TestSetInventorySourceSyncsAndInventoryIsServed(t *testing.T) {
	h := newHarness(t)
	h.pushInventory("c1")
	h.setInventorySource()

	inv := h.mustDo("GET", "/api/v1/inventory", nil, http.StatusOK)
	if inv["commit"] != "c1" {
		t.Errorf("commit = %v, want c1", inv["commit"])
	}
	if inv["activated_at"] == nil || inv["checked_at"] == nil {
		t.Errorf("activated_at/checked_at missing: %v", inv)
	}
	if envs, _ := inv["environments"].([]any); len(envs) != 5 {
		t.Errorf("environments = %v, want 5", inv["environments"])
	}
	services, _ := inv["services"].([]any)
	if len(services) != 2 {
		t.Fatalf("services = %v, want 2", inv["services"])
	}
	first := services[0].(map[string]any)
	if first["name"] != "backend-api" {
		t.Errorf("first service = %v, want backend-api (sorted)", first["name"])
	}
	if envs, _ := first["environments"].([]any); len(envs) == 0 {
		t.Errorf("service lists no environments: %v", first)
	}

	svc := h.mustDo("GET", "/api/v1/inventory/services/orders", nil, http.StatusOK)
	if svc["name"] != "orders" || svc["environments"] == nil {
		t.Errorf("service = %v, want the full orders service", svc)
	}
	if code, body := h.do("GET", "/api/v1/inventory/services/nope", nil); code != http.StatusNotFound || errorCode(t, body) != "not_found" {
		t.Errorf("unknown service = %d %v, want 404 not_found", code, body)
	}

	if code, body := h.do("PUT", "/api/v1/admin/inventory-source", map[string]string{"repo": "no-slash", "branch": "main"}); code != http.StatusBadRequest || errorCode(t, body) != "invalid" {
		t.Errorf("bad repo = %d %v, want 400 invalid", code, body)
	}

	// A new commit is picked up by the on-demand sync.
	h.pushInventory("c2")
	synced := h.mustDo("POST", "/api/v1/inventory/sync", nil, http.StatusOK)
	if synced["commit"] != "c2" {
		t.Errorf("after sync commit = %v, want c2", synced["commit"])
	}
}

func TestCorrectingTheInventoryPathActivatesAtTheSameHead(t *testing.T) {
	h := newHarness(t)
	h.pushInventory("c1")
	put := func(path string) map[string]any {
		return h.mustDo("PUT", "/api/v1/admin/inventory-source", map[string]string{"repo": invRepo, "branch": "main", "path": path}, http.StatusOK)
	}
	if inv := put("typo"); inv["commit"] != "" || inv["rejected"] == nil {
		t.Fatalf("typo path: commit %v rejected %v, want no commit and a problem", inv["commit"], inv["rejected"])
	}
	if inv := put(invPath); inv["commit"] != "c1" || inv["rejected"] != nil {
		t.Fatalf("corrected path: commit %v rejected %v, want c1 and none", inv["commit"], inv["rejected"])
	}

	// Moving to another path at the same head serves the new path's inventory.
	h.src.put(invRepo, "c1", "other/environments.yaml", fixture(t, "environments.yaml"))
	h.src.put(invRepo, "c1", "other/services/orders.yaml", fixture(t, "services/orders.yaml"))
	inv := put("other")
	if services, _ := inv["services"].([]any); len(services) != 1 {
		t.Fatalf("services after moving = %v, want only orders", inv["services"])
	}
}

func TestInventoryBeforeAnySourceIsEmptyNotAnError(t *testing.T) {
	h := newHarness(t)
	inv := h.mustDo("GET", "/api/v1/inventory", nil, http.StatusOK)
	if inv["commit"] != "" {
		t.Errorf("commit = %v, want empty", inv["commit"])
	}
	if code, _ := h.do("GET", "/api/v1/inventory/services/orders", nil); code != http.StatusNotFound {
		t.Errorf("service without inventory = %d, want 404", code)
	}
}

func TestRegisterJobSyncsAndReturnsDefinition(t *testing.T) {
	h := newHarness(t)
	h.pushInventory("c1")
	h.setInventorySource()
	h.pushJob("j1")
	folder := h.createFolder("Backends")

	job := h.registerJob(folder)
	if job["job_id"] != "deploy-backend" || job["runnable"] != true || job["commit"] != "j1" {
		t.Errorf("registered job = %v, want deploy-backend runnable at j1", job)
	}
	def, _ := job["definition"].(map[string]any)
	if def["id"] != "deploy-backend" || def["steps"] == nil {
		t.Errorf("definition = %v, want the parsed job", job["definition"])
	}
	if job["synced_at"] == nil {
		t.Errorf("synced_at missing: %v", job)
	}
	id := int64(job["id"].(float64))

	got := h.mustDo("GET", fmt.Sprintf("/api/v1/jobs/%d", id), nil, http.StatusOK)
	if got["definition"] == nil {
		t.Errorf("GET job has no definition: %v", got)
	}
	list := h.mustDo("GET", "/api/v1/jobs", nil, http.StatusOK)
	items, _ := list["jobs"].([]any)
	if len(items) != 1 {
		t.Fatalf("jobs = %v, want 1", list["jobs"])
	}
	row := items[0].(map[string]any)
	if row["folder"] != "Backends" || row["repo"] != jobsRepo || row["problem_count"] != float64(0) {
		t.Errorf("job row = %v", row)
	}
}

func TestRegisterSameFileTwiceIsConflict(t *testing.T) {
	h := newHarness(t)
	h.pushInventory("c1")
	h.setInventorySource()
	h.pushJob("j1")
	folder := h.createFolder("Backends")
	h.registerJob(folder)
	code, body := h.do("POST", "/api/v1/jobs", map[string]any{"folder_id": folder, "repo": jobsRepo, "branch": "main", "path": jobPath})
	if code != http.StatusConflict || errorCode(t, body) != "conflict" {
		t.Errorf("second register = %d %v, want 409 conflict", code, body)
	}
}

func TestMoveJobBetweenFolders(t *testing.T) {
	h := newHarness(t)
	h.pushInventory("c1")
	h.setInventorySource()
	h.pushJob("j1")
	from, to := h.createFolder("From"), h.createFolder("To")
	id := int64(h.registerJob(from)["id"].(float64))

	moved := h.mustDo("PATCH", fmt.Sprintf("/api/v1/jobs/%d", id), map[string]any{"folder_id": to}, http.StatusOK)
	if moved["folder_id"] != float64(to) || moved["folder"] != "To" {
		t.Errorf("moved job = %v, want folder To", moved)
	}

	code, body := h.do("PATCH", fmt.Sprintf("/api/v1/jobs/%d", id), map[string]any{"folder_id": 987654})
	if code != http.StatusBadRequest || errorCode(t, body) != "invalid" || !strings.Contains(errorMessage(body), "987654") {
		t.Errorf("move to unknown folder = %d %v, want 400 invalid naming 987654", code, body)
	}
	code, body = h.do("POST", "/api/v1/jobs", map[string]any{"folder_id": 987654, "repo": jobsRepo, "branch": "main", "path": "jobs/other.yaml"})
	if code != http.StatusBadRequest || errorCode(t, body) != "invalid" || !strings.Contains(errorMessage(body), "987654") {
		t.Errorf("register in unknown folder = %d %v, want 400 invalid naming 987654", code, body)
	}
}

func TestUnknownJobIs404(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1/jobs/4242"},
		{"PATCH", "/api/v1/jobs/4242"},
		{"POST", "/api/v1/jobs/4242/sync"},
		{"GET", "/api/v1/jobs/not-a-number"},
	} {
		var body any
		if tc.method == "PATCH" {
			body = map[string]any{"folder_id": 1}
		}
		code, got := h.do(tc.method, tc.path, body)
		if code != http.StatusNotFound || errorCode(t, got) != "not_found" {
			t.Errorf("%s %s = %d %v, want 404 not_found", tc.method, tc.path, code, got)
		}
	}
}

func TestSyncJobNowAndSourceErrorIsShown(t *testing.T) {
	h := newHarness(t)
	h.pushInventory("c1")
	h.setInventorySource()
	h.pushJob("j1")
	id := int64(h.registerJob(h.createFolder("Backends"))["id"].(float64))

	h.src.setErr(errors.New("github is down"))
	job := h.mustDo("POST", fmt.Sprintf("/api/v1/jobs/%d/sync", id), nil, http.StatusOK)
	msg, _ := job["sync_error"].(string)
	if !strings.Contains(msg, "github is down") || job["sync_error_at"] == nil {
		t.Errorf("job = %v, want sync_error and sync_error_at", job)
	}
	if job["commit"] != "j1" || job["definition"] == nil {
		t.Errorf("last good definition lost during outage: %v", job)
	}

	inv := h.mustDo("POST", "/api/v1/inventory/sync", nil, http.StatusOK)
	if m, _ := inv["sync_error"].(string); !strings.Contains(m, "github is down") || inv["sync_error_at"] == nil {
		t.Errorf("inventory = %v, want sync_error and sync_error_at", inv)
	}
	if inv["commit"] != "c1" || inv["activated_at"] == nil {
		t.Errorf("age of last good sync lost: %v", inv)
	}

	h.src.setErr(nil)
	job = h.mustDo("POST", fmt.Sprintf("/api/v1/jobs/%d/sync", id), nil, http.StatusOK)
	if job["sync_error"] != "" {
		t.Errorf("sync_error = %v after recovery, want empty", job["sync_error"])
	}
}

func TestInventoryShowsProblemsOfRejectedCommitAlongsideActiveOne(t *testing.T) {
	h := newHarness(t)
	h.pushInventory("c1")
	h.setInventorySource()

	// c2 has a broken service file.
	h.pushInventory("c2")
	h.src.put(invRepo, "c2", invPath+"/services/orders.yaml", []byte("apiVersion: yumdrake/v1\nkind: Service\nname: orders\nbogus: true\n"))
	inv := h.mustDo("POST", "/api/v1/inventory/sync", nil, http.StatusOK)

	if inv["commit"] != "c1" {
		t.Errorf("active commit = %v, want c1 to stay active", inv["commit"])
	}
	rejected, _ := inv["rejected"].(map[string]any)
	if rejected["commit"] != "c2" {
		t.Fatalf("rejected = %v, want commit c2", inv["rejected"])
	}
	problems, _ := rejected["problems"].([]any)
	if len(problems) == 0 {
		t.Fatalf("rejected has no problems: %v", rejected)
	}
	first := problems[0].(map[string]any)
	if first["file"] != "services/orders.yaml" || first["line"] == nil || first["message"] == "" {
		t.Errorf("problem = %v, want file, line and message", first)
	}
}

func TestUnknownJSONFieldIsInvalid(t *testing.T) {
	h := newHarness(t)
	code, body := h.do("POST", "/api/v1/folders", `{"name":"x","surprise":true}`)
	if code != http.StatusBadRequest || errorCode(t, body) != "invalid" {
		t.Errorf("unknown field = %d %v, want 400 invalid", code, body)
	}
	code, body = h.do("POST", "/api/v1/folders", `{"name":"x"} {"name":"y"}`)
	if code != http.StatusBadRequest || errorCode(t, body) != "invalid" {
		t.Errorf("trailing data = %d %v, want 400 invalid", code, body)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	h := newHarness(t)
	huge := `{"name":"` + strings.Repeat("a", 2<<20) + `"}`
	code, body := h.do("POST", "/api/v1/folders", huge)
	if code != http.StatusRequestEntityTooLarge || errorCode(t, body) != "invalid" {
		t.Errorf("oversized body = %d %v, want 413 invalid", code, body)
	}
}

func TestFoldersCreateListAndRejectDuplicates(t *testing.T) {
	h := newHarness(t)
	h.createFolder("Payments")
	if code, body := h.do("POST", "/api/v1/folders", map[string]string{"name": "payments"}); code != http.StatusConflict || errorCode(t, body) != "conflict" {
		t.Errorf("duplicate folder = %d %v, want 409 conflict", code, body)
	}
	if code, body := h.do("POST", "/api/v1/folders", map[string]string{"name": "  "}); code != http.StatusBadRequest || errorCode(t, body) != "invalid" {
		t.Errorf("blank folder = %d %v, want 400 invalid", code, body)
	}
	list := h.mustDo("GET", "/api/v1/folders", nil, http.StatusOK)
	if folders, _ := list["folders"].([]any); len(folders) != 1 {
		t.Errorf("folders = %v, want 1", list["folders"])
	}
}

// stubSyncer returns fixed errors from the on-demand syncs.
type stubSyncer struct{ err error }

func (s stubSyncer) SyncInventory(context.Context) error { return s.err }
func (s stubSyncer) SetInventorySource(context.Context, store.InventorySource, func(context.Context, store.DBTX) error) error {
	return s.err
}
func (s stubSyncer) SyncJob(context.Context, int64) error { return s.err }

func (h *harness) withSyncer(syncer Syncer) {
	h.rebuild(func(d *Deps) { d.Engine = syncer })
}

func TestInternalSyncFailureIsInternalError(t *testing.T) {
	h := newHarness(t)
	h.withSyncer(stubSyncer{err: errors.New("pq: connection to 10.0.0.5 refused")})
	code, body := h.do("POST", "/api/v1/inventory/sync", nil)
	if code != http.StatusInternalServerError || errorCode(t, body) != "internal" {
		t.Fatalf("inventory sync = %d %v, want 500 internal", code, body)
	}
	if strings.Contains(errorMessage(body), "10.0.0.5") {
		t.Errorf("the internal detail leaked into the response: %v", body)
	}

	folder := h.createFolder("Backends")
	code, body = h.do("POST", "/api/v1/jobs", map[string]any{"folder_id": folder, "repo": jobsRepo, "branch": "main", "path": jobPath})
	if code != http.StatusInternalServerError || errorCode(t, body) != "internal" {
		t.Errorf("register job with failing sync = %d %v, want 500 internal", code, body)
	}
}

func TestSourceSyncFailureIsShownNotFailed(t *testing.T) {
	h := newHarness(t)
	h.withSyncer(stubSyncer{err: fmt.Errorf("read branch: %w", yumsync.ErrSource)})
	if code, body := h.do("POST", "/api/v1/inventory/sync", nil); code != http.StatusOK {
		t.Errorf("inventory sync with a source error = %d %v, want 200", code, body)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// shortenValkeyTimeout makes a dead Valkey answer quickly in a test.
func shortenValkeyTimeout(t *testing.T) {
	t.Helper()
	previous := valkeyTimeout
	valkeyTimeout = 500 * time.Millisecond
	t.Cleanup(func() { valkeyTimeout = previous })
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return b
}
