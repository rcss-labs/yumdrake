package fakegithub_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/rcss-labs/yamdrake/internal/github"
	"github.com/rcss-labs/yamdrake/test/e2e/fakegithub"
)

const (
	token = "fake-token"
	repo  = "acme/platform"
)

func newFake(t *testing.T) (*fakegithub.Server, *github.Client) {
	t.Helper()
	srv := fakegithub.New(token)
	t.Cleanup(srv.Close)
	return srv, clientFor(t, srv, token)
}

func clientFor(t *testing.T, srv *fakegithub.Server, tok string) *github.Client {
	t.Helper()
	c, err := github.New(srv.URL(), nil, github.Credentials{Method: "token", Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHeadCommitFollowsSetHead(t *testing.T) {
	srv, c := newFake(t)
	ctx := context.Background()
	srv.AddCommit(repo, "aaa111", map[string][]byte{"a.yaml": []byte("a")})
	srv.AddCommit(repo, "bbb222", map[string][]byte{"a.yaml": []byte("b")})
	srv.SetHead(repo, "main", "aaa111")
	srv.SetHead(repo, "feat/x", "bbb222")

	for branch, want := range map[string]string{"main": "aaa111", "feat/x": "bbb222"} {
		got, err := c.HeadCommit(ctx, repo, branch)
		if err != nil || got != want {
			t.Fatalf("HeadCommit(%s) = %q, %v; want %q", branch, got, err, want)
		}
	}
	srv.SetHead(repo, "main", "bbb222")
	if got, err := c.HeadCommit(ctx, repo, "main"); err != nil || got != "bbb222" {
		t.Fatalf("after SetHead HeadCommit = %q, %v", got, err)
	}
	if _, err := c.HeadCommit(ctx, repo, "nope"); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("unknown branch err = %v, want ErrNotFound", err)
	}
	if ok, err := c.BranchExists(ctx, repo, "main"); err != nil || !ok {
		t.Fatalf("BranchExists(main) = %v, %v", ok, err)
	}
	if ok, err := c.BranchExists(ctx, repo, "nope"); err != nil || ok {
		t.Fatalf("BranchExists(nope) = %v, %v", ok, err)
	}
}

func TestListFilesRecursiveAndScoped(t *testing.T) {
	srv, c := newFake(t)
	ctx := context.Background()
	if err := srv.LoadDir(repo, "c1", "testdata/c1"); err != nil {
		t.Fatal(err)
	}
	all, err := c.ListFiles(ctx, repo, "c1", "")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(all)
	want := []string{"environments.yaml", "jobs/deploy.yaml", "services/orders.yaml"}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("all = %v, want %v", all, want)
	}
	scoped, err := c.ListFiles(ctx, repo, "c1", "services")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scoped, []string{"services/orders.yaml"}) {
		t.Fatalf("scoped = %v", scoped)
	}
	if _, err := c.ListFiles(ctx, repo, "missing", ""); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("unknown commit err = %v, want ErrNotFound", err)
	}
	if _, err := c.ListFiles(ctx, repo, "c1", "no-such-dir"); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("unknown dir err = %v, want ErrNotFound", err)
	}
}

func TestReadFileAtCommit(t *testing.T) {
	srv, c := newFake(t)
	ctx := context.Background()
	long := make([]byte, 500) // forces base64 line wrapping
	for i := range long {
		long[i] = byte('a' + i%26)
	}
	srv.AddCommit(repo, "v1", map[string][]byte{"dir/f.yaml": []byte("one"), "big.txt": long})
	srv.AddCommit(repo, "v2", map[string][]byte{"dir/f.yaml": []byte("two")})

	for sha, want := range map[string]string{"v1": "one", "v2": "two"} {
		got, err := c.ReadFile(ctx, repo, sha, "dir/f.yaml")
		if err != nil || string(got) != want {
			t.Fatalf("ReadFile@%s = %q, %v; want %q", sha, got, err, want)
		}
	}
	if got, err := c.ReadFile(ctx, repo, "v1", "big.txt"); err != nil || string(got) != string(long) {
		t.Fatalf("big file mismatch, err = %v", err)
	}
	if _, err := c.ReadFile(ctx, repo, "v2", "big.txt"); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("file absent at commit err = %v, want ErrNotFound", err)
	}
	if _, err := c.ReadFile(ctx, repo, "v1", "dir"); err == nil {
		t.Fatal("reading a directory should fail")
	}
}

func TestRepeatRequestsReuseCacheViaNotModified(t *testing.T) {
	srv, c := newFake(t)
	ctx := context.Background()
	srv.AddCommit(repo, "aaa", map[string][]byte{"a.yaml": []byte("a")})
	srv.AddCommit(repo, "bbb", map[string][]byte{"a.yaml": []byte("b")})
	srv.SetHead(repo, "main", "aaa")

	for i := 0; i < 3; i++ {
		if got, err := c.HeadCommit(ctx, repo, "main"); err != nil || got != "aaa" {
			t.Fatalf("HeadCommit #%d = %q, %v", i, got, err)
		}
		if files, err := c.ListFiles(ctx, repo, "aaa", ""); err != nil || len(files) != 1 {
			t.Fatalf("ListFiles #%d = %v, %v", i, files, err)
		}
	}
	if n := srv.NotModified(); n != 4 {
		t.Fatalf("304 responses = %d, want 4 (2 endpoints x 2 repeats)", n)
	}
	// A moved head must not be masked by a stale cached 304.
	srv.SetHead(repo, "main", "bbb")
	if got, err := c.HeadCommit(ctx, repo, "main"); err != nil || got != "bbb" {
		t.Fatalf("HeadCommit after move = %q, %v", got, err)
	}
}

func TestUnavailableFailsAndRecovers(t *testing.T) {
	srv, c := newFake(t)
	ctx := context.Background()
	srv.AddCommit(repo, "aaa", map[string][]byte{"a.yaml": []byte("a")})
	srv.SetHead(repo, "main", "aaa")

	srv.SetUnavailable(true)
	_, err := c.HeadCommit(ctx, repo, "main")
	if err == nil || errors.Is(err, github.ErrNotFound) {
		t.Fatalf("HeadCommit while unavailable err = %v, want non-NotFound error", err)
	}
	if _, err := c.ListFiles(ctx, repo, "aaa", ""); err == nil || errors.Is(err, github.ErrNotFound) {
		t.Fatalf("ListFiles while unavailable err = %v", err)
	}
	if _, err := c.ReadFile(ctx, repo, "aaa", "a.yaml"); err == nil || errors.Is(err, github.ErrNotFound) {
		t.Fatalf("ReadFile while unavailable err = %v", err)
	}
	if _, err := c.BranchExists(ctx, repo, "main"); err == nil {
		t.Fatal("BranchExists while unavailable should error")
	}
	srv.SetUnavailable(false)
	if got, err := c.HeadCommit(ctx, repo, "main"); err != nil || got != "aaa" {
		t.Fatalf("after recovery = %q, %v", got, err)
	}
}

func TestWrongTokenIsRejected(t *testing.T) {
	srv, _ := newFake(t)
	srv.AddCommit(repo, "aaa", nil)
	srv.SetHead(repo, "main", "aaa")
	bad := clientFor(t, srv, "wrong")
	_, err := bad.HeadCommit(context.Background(), repo, "main")
	if err == nil || errors.Is(err, github.ErrNotFound) {
		t.Fatalf("wrong token err = %v, want a non-NotFound error", err)
	}
}

func TestRequestCountersAndConcurrency(t *testing.T) {
	srv, c := newFake(t)
	ctx := context.Background()
	srv.AddCommit(repo, "aaa", map[string][]byte{"a.yaml": []byte("a")})
	srv.SetHead(repo, "main", "aaa")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.HeadCommit(ctx, repo, "main")
			_, _ = c.ReadFile(ctx, repo, "aaa", "a.yaml")
			srv.SetHead(repo, "main", "aaa")
		}()
	}
	wg.Wait()
	if n := srv.Requests(fakegithub.EndpointCommits); n != 8 {
		t.Errorf("commits requests = %d, want 8", n)
	}
	if n := srv.Requests(fakegithub.EndpointContents); n != 8 {
		t.Errorf("contents requests = %d, want 8", n)
	}
	if n := srv.Requests(fakegithub.EndpointTrees); n != 0 {
		t.Errorf("trees requests = %d, want 0", n)
	}
}

// A caller that owns the listener (the demo) serves the fake itself, with no
// httptest server started for it.
func TestHandlerServesWithoutOwnListener(t *testing.T) {
	srv := fakegithub.NewUnstarted(token)
	srv.AddCommit(repo, "aaa111", map[string][]byte{"environments.yaml": []byte("x")})
	srv.SetHead(repo, "main", "aaa111")

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c, err := github.New(ts.URL, nil, github.Credentials{Method: "token", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.HeadCommit(context.Background(), repo, "main"); err != nil || got != "aaa111" {
		t.Fatalf("HeadCommit = %q, %v; want aaa111", got, err)
	}
	if srv.URL() != "" {
		t.Errorf("URL() = %q, want empty for an unstarted fake", srv.URL())
	}
	srv.Close() // must be safe with nothing started
}
