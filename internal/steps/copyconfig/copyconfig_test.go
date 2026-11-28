package copyconfig_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner/runnertest"
	"github.com/rcss-labs/yamdrake/internal/steps/copyconfig"
	"github.com/rcss-labs/yamdrake/internal/store"
)

const (
	serviceARN = "arn:aws:ecs:eu-west-1:123456789012:service/cluster-1/web"
	marker     = "ZXQ-SECRET-MARKER-9917"
)

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// fakeS3 is an in-memory S3 keyed "bucket/key".
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []string
	// putErr, when set, is called for each PutObject (n counts from 1); a
	// non-nil result is returned and nothing is stored.
	putErr func(n int, key string) error
	// putThenErr stores the object and still returns the error (a lost reply).
	putThenErr func(n int, key string) error
	// mangle stores something other than what was sent (a bad read-back).
	mangle bool
	getErr func(key string) error
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := aws.ToString(in.Bucket) + "/" + aws.ToString(in.Key)
	if f.getErr != nil {
		if err := f.getErr(key); err != nil {
			return nil, err
		}
	}
	data, ok := f.objects[key]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := aws.ToString(in.Bucket) + "/" + aws.ToString(in.Key)
	f.puts = append(f.puts, key)
	n := len(f.puts)
	if f.putErr != nil {
		if err := f.putErr(n, key); err != nil {
			return nil, err
		}
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	if f.mangle {
		data = append(data, '!')
	}
	f.objects[key] = data
	if f.putThenErr != nil {
		if err := f.putThenErr(n, key); err != nil {
			return nil, err
		}
	}
	return &s3.PutObjectOutput{}, nil
}

type fakeArtifacts struct{ objects map[string][]byte }

func (f fakeArtifacts) Get(_ context.Context, key, want string) ([]byte, error) {
	data, ok := f.objects[key]
	if !ok {
		return nil, errors.New("no such artifact")
	}
	if sum(data) != want {
		return nil, fmt.Errorf("%w: %s", artifacts.ErrCorrupt, key)
	}
	return data, nil
}

// refusal is what the SDK returns for an HTTP 4xx answer.
func refusal(status int, code, message string) error {
	return &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
		Err:      &smithy.GenericAPIError{Code: code, Message: message},
	}}
}

type file struct{ name, dest, content string }

type fixture struct {
	t       *testing.T
	s3      *fakeS3
	art     fakeArtifacts
	env     *runnertest.Env
	items   []manifest.ConfigItem
	task    queue.Claimed
	awsErr  error
	account string
}

func newFixture(t *testing.T, files ...file) *fixture {
	t.Helper()
	f := &fixture{t: t, s3: newFakeS3(), art: fakeArtifacts{objects: map[string][]byte{}}, env: &runnertest.Env{Attempt: 2}, account: "123456789012"}
	for _, file := range files {
		data := []byte(file.content)
		key := "art/" + file.name
		f.art.objects[key] = data
		f.items = append(f.items, manifest.ConfigItem{
			Kind: "env", Name: file.name, Destination: "s3://dest-bucket/" + file.dest,
			SHA256: sum(data), ArtifactKey: key, Size: int64(len(data)),
		})
	}
	return f
}

func (f *fixture) dest(name string) string {
	for _, item := range f.items {
		if item.Name == name {
			return strings.TrimPrefix(item.Destination, "s3://")
		}
	}
	f.t.Fatalf("no item %q", name)
	return ""
}

func (f *fixture) run() queue.Outcome {
	f.t.Helper()
	m := manifest.Manifest{Services: []manifest.Service{{Name: "web", ECSService: serviceARN, Config: f.items}}}
	raw, err := json.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	f.task = queue.Claimed{
		Leg: store.Leg{ID: 7, RunID: 42, Kind: "step", Service: "web", Attempt: f.env.Attempt},
		Run: store.Run{ID: 42, AWSAccount: f.account, Manifest: raw},
	}
	x := copyconfig.New(copyconfig.Deps{
		AWS: func(_ context.Context, account, region, session string) (aws.Config, error) {
			if f.awsErr != nil {
				return aws.Config{}, f.awsErr
			}
			if region != "eu-west-1" || session != "yumdrake-runner-run-42" {
				f.t.Errorf("AWS called with region %q session %q", region, session)
			}
			return aws.Config{}, nil
		},
		S3:        func(aws.Config) copyconfig.S3API { return f.s3 },
		Artifacts: f.art,
	})
	return x.Execute(context.Background(), f.env, f.task)
}

func result(t *testing.T, out queue.Outcome) copyconfig.Result {
	t.Helper()
	var r copyconfig.Result
	if err := json.Unmarshal(out.Result, &r); err != nil {
		t.Fatalf("result %q: %v", out.Result, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out.Result, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"written", "already_in_place", "not_written", "not_restored"} {
		if string(raw[k]) == "null" || raw[k] == nil {
			t.Errorf("result list %s is %s, want []", k, raw[k])
		}
	}
	return r
}

func logText(env *runnertest.Env) string {
	var b strings.Builder
	for _, l := range env.Lines() {
		b.WriteString(l.Level + ": " + l.Text + "\n")
	}
	return b.String()
}

func want(t *testing.T, label string, got []string, wantList ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(wantList, ",") {
		t.Errorf("%s = %v, want %v", label, got, wantList)
	}
}

func uri(f *fixture, names ...string) []string {
	var out []string
	for _, n := range names {
		out = append(out, "s3://"+f.dest(n))
	}
	return out
}

func TestCopiesEveryItemAndVerifies(t *testing.T) {
	f := newFixture(t, file{"web.env", "web.env", "A=1\n"}, file{"web.toml", "web.toml", "x = 1\n"})
	out := f.run()
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q: %s\n%s", out.State, out.FailureShort, out.FailureDetail, logText(f.env))
	}
	r := result(t, out)
	want(t, "written", r.Written, uri(f, "web.env", "web.toml")...)
	for _, item := range f.items {
		if got := f.s3.objects[strings.TrimPrefix(item.Destination, "s3://")]; sum(got) != item.SHA256 {
			t.Errorf("%s holds the wrong bytes", item.Name)
		}
	}
	ops := f.env.OpsSnapshot()
	if len(ops) != 2 || ops[0].Name != "put-config" || ops[0].Key != "put-config:"+f.items[0].Destination || ops[0].State != "done" {
		t.Errorf("ops = %+v", ops)
	}
	if !strings.Contains(logText(f.env), "Wrote 2, already in place 0, not written 0, not restored 0.") {
		t.Errorf("no summary line:\n%s", logText(f.env))
	}
}

func TestSkipsFilesAlreadyInPlace(t *testing.T) {
	f := newFixture(t, file{"web.env", "web.env", "A=1\n"}, file{"web.toml", "web.toml", "x = 1\n"})
	f.s3.objects[f.dest("web.env")] = []byte("A=1\n")
	out := f.run()
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	r := result(t, out)
	want(t, "already", r.AlreadyInPlace, uri(f, "web.env")...)
	want(t, "written", r.Written, uri(f, "web.toml")...)
	if len(f.s3.puts) != 1 || len(f.env.OpsSnapshot()) != 1 {
		t.Errorf("puts = %v, ops = %d; the file in place must not be written", f.s3.puts, len(f.env.OpsSnapshot()))
	}
	if !strings.Contains(logText(f.env), "web.env: already in place") {
		t.Errorf("log does not say web.env was in place:\n%s", logText(f.env))
	}
}

func TestDeniedHalfwayReportsWrittenAndNotWritten(t *testing.T) {
	f := newFixture(t, file{"a.env", "a.env", "A=1\n"}, file{"b.env", "b.env", "B=1\n"}, file{"c.env", "c.env", "C=1\n"})
	f.s3.putErr = func(n int, _ string) error {
		if n == 2 {
			return refusal(403, "AccessDenied", "User arn:aws:iam::1:user/x is not allowed")
		}
		return nil
	}
	out := f.run()
	if out.State != "failed" || out.FailureShort != "AccessDenied on s3:PutObject" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	r := result(t, out)
	want(t, "written", r.Written, uri(f, "a.env")...)
	want(t, "not written", r.NotWritten, uri(f, "b.env", "c.env")...)
	for _, name := range []string{"a.env", "b.env", "c.env"} {
		if !strings.Contains(out.FailureDetail, name) {
			t.Errorf("detail does not name %s: %s", name, out.FailureDetail)
		}
	}
	if !strings.Contains(logText(f.env), "b.env") || !strings.Contains(logText(f.env), "not written") {
		t.Errorf("log does not list the files not written:\n%s", logText(f.env))
	}
	if len(f.s3.puts) != 2 {
		t.Errorf("puts = %v; nothing after the refusal may be written", f.s3.puts)
	}
}

func TestSecondAttemptWritesOnlyMissing(t *testing.T) {
	f := newFixture(t, file{"a.env", "a.env", "A=1\n"}, file{"b.env", "b.env", "B=1\n"}, file{"c.env", "c.env", "C=1\n"})
	// First attempt: a is written, the reply to b's write is lost and the write
	// never happened.
	f.s3.putErr = func(n int, _ string) error {
		if n == 2 {
			return errors.New("connection reset by peer")
		}
		return nil
	}
	f.env.Attempt = 1
	out := f.run()
	if out.State != "unknown" {
		t.Fatalf("first attempt = %q %q", out.State, out.FailureShort)
	}
	first := len(f.s3.puts)

	f.s3.putErr = nil
	f.env.Attempt = 2
	out = f.run()
	if out.State != "succeeded" {
		t.Fatalf("second attempt = %q %q: %s\n%s", out.State, out.FailureShort, out.FailureDetail, logText(f.env))
	}
	r := result(t, out)
	want(t, "already", r.AlreadyInPlace, uri(f, "a.env")...)
	want(t, "written", r.Written, uri(f, "b.env", "c.env")...)
	for _, put := range f.s3.puts[first:] {
		if put == f.dest("a.env") {
			t.Errorf("the file already in place was written again: %v", f.s3.puts)
		}
	}
	if len(f.s3.puts)-first != 2 {
		t.Errorf("second attempt made %d writes, want 2", len(f.s3.puts)-first)
	}
}

func TestAmbiguousPutIsResolvedByReadingDestination(t *testing.T) {
	intended := func(f *fixture) {
		f.env.PresetOps = []store.Op{{ID: 1, Name: "put-config", Key: "put-config:" + f.items[0].Destination, State: "intended"}}
	}
	t.Run("destination holds the manifest's content", func(t *testing.T) {
		f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
		intended(f)
		f.s3.objects[f.dest("web.env")] = []byte("A=1\n")
		out := f.run()
		if out.State != "succeeded" {
			t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
		}
		r := result(t, out)
		want(t, "written", r.Written, uri(f, "web.env")...)
		if len(f.s3.puts) != 0 {
			t.Errorf("wrote again: %v", f.s3.puts)
		}
		if !strings.Contains(logText(f.env), "earlier attempt") {
			t.Errorf("log does not say an earlier attempt's write is in place:\n%s", logText(f.env))
		}
	})
	t.Run("destination differs", func(t *testing.T) {
		f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
		intended(f)
		f.s3.objects[f.dest("web.env")] = []byte("OLD=1\n")
		out := f.run()
		if out.State != "succeeded" {
			t.Fatalf("outcome = %q %q: %s", out.State, out.FailureShort, out.FailureDetail)
		}
		want(t, "written", result(t, out).Written, uri(f, "web.env")...)
		if string(f.s3.objects[f.dest("web.env")]) != "A=1\n" {
			t.Errorf("destination not written")
		}
		keys := f.env.DoKeys()
		wantKey := "put-config:" + f.items[0].Destination + ":retry-2"
		if len(keys) != 2 || keys[1] != wantKey {
			t.Errorf("Do keys = %v, want the second to be %q", keys, wantKey)
		}
	})
	t.Run("destination missing", func(t *testing.T) {
		f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
		intended(f)
		out := f.run()
		if out.State != "succeeded" || len(f.s3.puts) != 1 {
			t.Fatalf("outcome = %q %q, puts %v", out.State, out.FailureShort, f.s3.puts)
		}
	})
}

func TestCancelBetweenFilesReturnsCancelledWithResult(t *testing.T) {
	f := newFixture(t, file{"a.env", "a.env", "A=1\n"}, file{"b.env", "b.env", "B=1\n"}, file{"c.env", "c.env", "C=1\n"})
	f.env.AfterDo = func(e *runnertest.Env) { e.SetCancel(true) }
	out := f.run()
	if out.State != "cancelled" || out.FailureShort != "cancelled after 1 of 3 files" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	r := result(t, out)
	want(t, "written", r.Written, uri(f, "a.env")...)
	want(t, "not written", r.NotWritten, uri(f, "b.env", "c.env")...)
	if len(f.s3.puts) != 1 {
		t.Errorf("puts = %v", f.s3.puts)
	}
}

func TestCancelBeforeFirstWriteWritesNothing(t *testing.T) {
	f := newFixture(t, file{"a.env", "a.env", "A=1\n"})
	f.env.Cancel = true
	out := f.run()
	if out.State != "cancelled" || len(f.s3.puts) != 0 {
		t.Fatalf("outcome = %q %q, puts %v", out.State, out.FailureShort, f.s3.puts)
	}
	want(t, "not written", result(t, out).NotWritten, uri(f, "a.env")...)
}

func TestCancelWithInheritedIntentReconcilesAndDoesNotEndCancelled(t *testing.T) {
	setup := func(t *testing.T, inPlace bool) *fixture {
		f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
		f.env.Cancel = true
		f.env.PresetOps = []store.Op{{ID: 1, Name: "put-config", Key: "put-config:" + f.items[0].Destination, State: "intended"}}
		if inPlace {
			f.s3.objects[f.dest("web.env")] = []byte("A=1\n")
		}
		return f
	}
	t.Run("the intended write is in place", func(t *testing.T) {
		f := setup(t, true)
		out := f.run()
		if out.State != "succeeded" {
			t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
		}
		want(t, "written", result(t, out).Written, uri(f, "web.env")...)
	})
	t.Run("the intended write is not in place", func(t *testing.T) {
		f := setup(t, false)
		out := f.run()
		if out.State != "failed" || out.FailureShort != "cancelled; 1 files not written" {
			t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
		}
		want(t, "not written", result(t, out).NotWritten, uri(f, "web.env")...)
		if len(f.s3.puts) != 0 || len(f.env.DoKeys()) != 0 {
			t.Errorf("a write was attempted: puts %v, Do %v", f.s3.puts, f.env.DoKeys())
		}
	})
}

func TestCorruptArtifactFailsWithoutWriting(t *testing.T) {
	f := newFixture(t, file{"a.env", "a.env", "A=1\n"}, file{"b.env", "b.env", "B=1\n"})
	f.art.objects["art/a.env"] = []byte("TAMPERED\n")
	out := f.run()
	if out.State != "failed" || out.FailureShort != "stored copy of a.env is not usable" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	if len(f.s3.puts) != 0 || len(f.env.OpsSnapshot()) != 0 {
		t.Errorf("something was written: %v", f.s3.puts)
	}
	want(t, "not written", result(t, out).NotWritten, uri(f, "a.env", "b.env")...)
}

func TestNoConfigSucceeds(t *testing.T) {
	f := newFixture(t)
	out := f.run()
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	found := false
	for _, l := range f.env.Lines() {
		if l.Text == "no env files or TOMLs for this service, nothing to copy" {
			found = true
		}
	}
	if !found {
		t.Errorf("line missing:\n%s", logText(f.env))
	}
	result(t, out)
}

func TestNotRestoredItemsAreSkippedAndListed(t *testing.T) {
	f := newFixture(t, file{"a.env", "a.env", "A=1\n"}, file{"b.env", "b.env", "B=1\n"})
	f.items[0].NotRestored = "the file did not exist before the deploy"
	out := f.run()
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	r := result(t, out)
	want(t, "not restored", r.NotRestored, uri(f, "a.env")...)
	want(t, "written", r.Written, uri(f, "b.env")...)
	if _, ok := f.s3.objects[f.dest("a.env")]; ok {
		t.Error("a not-restored file was written")
	}
	if !strings.Contains(logText(f.env), "a.env: not restored: the file did not exist before the deploy") {
		t.Errorf("log:\n%s", logText(f.env))
	}
}

func TestRefusalIsFailedButUncertainWriteIsUnknown(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeS3)
		state string
		short string
	}{
		{"a 403 refusal", func(s *fakeS3) { s.putErr = func(int, string) error { return refusal(403, "AccessDenied", "no") } }, "failed", "AccessDenied on s3:PutObject"},
		{"a transport error", func(s *fakeS3) { s.putErr = func(int, string) error { return errors.New("connection reset") } }, "unknown", "could not confirm the write of web.env"},
		{"a 500 answer", func(s *fakeS3) { s.putErr = func(int, string) error { return refusal(500, "InternalError", "no") } }, "unknown", "could not confirm the write of web.env"},
		{"a lost reply after the write", func(s *fakeS3) { s.putThenErr = func(int, string) error { return errors.New("EOF") } }, "unknown", "could not confirm the write of web.env"},
		{"a read-back that differs", func(s *fakeS3) { s.mangle = true }, "unknown", "could not confirm the write of web.env"},
		{"a read-back that fails", func(s *fakeS3) {
			n := 0
			s.getErr = func(string) error {
				n++
				if n == 2 { // the first read finds the destination, the second is the read-back
					return refusal(503, "SlowDown", "x")
				}
				return nil
			}
		}, "unknown", "could not confirm the write of web.env"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
			c.setup(f.s3)
			out := f.run()
			if out.State != c.state || out.FailureShort != c.short {
				t.Fatalf("outcome = %q %q, want %q %q", out.State, out.FailureShort, c.state, c.short)
			}
			want(t, "not written", result(t, out).NotWritten, uri(f, "web.env")...)
			ops := f.env.OpsSnapshot()
			wantState := map[string]string{"failed": "failed", "unknown": "intended"}[c.state]
			if len(ops) != 1 || ops[0].State != wantState {
				t.Errorf("ops = %+v, want one %s op", ops, wantState)
			}
		})
	}
}

func TestPreviouslyFailedIsNotWrittenAgain(t *testing.T) {
	failedOp := func(f *fixture) {
		f.env.PresetOps = []store.Op{{ID: 1, Name: "put-config", Key: "put-config:" + f.items[0].Destination, State: "failed"}}
	}
	t.Run("destination differs", func(t *testing.T) {
		f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
		failedOp(f)
		out := f.run()
		if out.State != "failed" || out.FailureShort != "an earlier attempt's s3:PutObject of web.env was refused" {
			t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
		}
		if len(f.s3.puts) != 0 {
			t.Errorf("wrote again: %v", f.s3.puts)
		}
		want(t, "not written", result(t, out).NotWritten, uri(f, "web.env")...)
	})
	t.Run("destination already holds the content", func(t *testing.T) {
		f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
		failedOp(f)
		f.s3.objects[f.dest("web.env")] = []byte("A=1\n")
		out := f.run()
		if out.State != "succeeded" {
			t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
		}
		want(t, "already", result(t, out).AlreadyInPlace, uri(f, "web.env")...)
	})
}

func TestCredentialsFailureFailsBeforeTouchingAnything(t *testing.T) {
	f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
	f.awsErr = errors.New("sts said no")
	out := f.run()
	if out.State != "failed" || out.FailureShort != "could not get credentials for account 123456789012" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	if len(f.s3.puts) != 0 {
		t.Error("wrote after a credentials failure")
	}
}

func TestLogNeverContainsFileContents(t *testing.T) {
	secret := marker + "=hunter2\n"
	f := newFixture(t, file{"a.env", "a.env", secret}, file{"b.env", "b.env", secret + "B=1\n"}, file{"c.env", "c.env", secret + "C=1\n"})
	f.s3.objects[f.dest("a.env")] = []byte("OLD-" + marker)
	f.s3.putErr = func(n int, _ string) error {
		if n == 2 {
			return refusal(403, "AccessDenied", "cannot write "+marker)
		}
		return nil
	}
	out := f.run()
	if out.State != "failed" || out.FailureShort != "AccessDenied on s3:PutObject" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	check := func(label, text string) {
		if strings.Contains(text, marker) || strings.Contains(text, "hunter2") {
			t.Errorf("%s leaks file content or an AWS message: %s", label, text)
		}
	}
	for _, l := range f.env.Lines() {
		check("log line", l.Text)
	}
	check("short", out.FailureShort)
	check("detail", out.FailureDetail)
	check("result", string(out.Result))
	for _, op := range f.env.OpsSnapshot() {
		check("operation intent", string(op.Intent))
	}
}

func inheritedIntent(f *fixture) {
	f.env.PresetOps = []store.Op{{ID: 1, Name: "put-config", Key: "put-config:" + f.items[0].Destination, State: "intended"}}
}

func TestCredentialsFailureWithInheritedIntentIsUnknown(t *testing.T) {
	f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
	inheritedIntent(f)
	f.awsErr = errors.New("sts said no")
	out := f.run()
	if out.State != "unknown" || out.FailureShort != "could not get credentials for account 123456789012" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
}

func TestReadErrorWithInheritedIntentIsUnknown(t *testing.T) {
	f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
	inheritedIntent(f)
	f.s3.getErr = func(string) error { return refusal(503, "SlowDown", "x") }
	out := f.run()
	if out.State != "unknown" || out.FailureShort != "could not check an interrupted write of web.env" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
	if len(f.s3.puts) != 0 {
		t.Errorf("wrote: %v", f.s3.puts)
	}
}

func TestReadErrorWithoutIntentIsFailed(t *testing.T) {
	f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
	f.s3.getErr = func(string) error { return refusal(403, "AccessDenied", "x") }
	out := f.run()
	if out.State != "failed" || out.FailureShort != "AccessDenied on s3:GetObject" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
}

func TestUnusableStoredCopyWithInheritedIntentIsUnknown(t *testing.T) {
	f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
	inheritedIntent(f)
	f.art.objects["art/web.env"] = []byte("TAMPERED\n")
	out := f.run()
	if out.State != "unknown" || out.FailureShort != "could not check an interrupted write of web.env" {
		t.Fatalf("outcome = %q %q", out.State, out.FailureShort)
	}
}

// laterIntentFixture has b.env before a.env; an earlier attempt left a.env's
// write intended, and b.env's write is now refused.
func laterIntentFixture(t *testing.T) *fixture {
	f := newFixture(t, file{"b.env", "b.env", "B=1\n"}, file{"a.env", "a.env", "A=1\n"})
	f.env.PresetOps = []store.Op{{ID: 1, Name: "put-config", Key: "put-config:" + f.items[1].Destination, State: "intended"}}
	f.s3.putErr = func(int, string) error { return refusal(403, "AccessDenied", "x") }
	return f
}

func TestEarlierFileFailureSettlesLaterInheritedIntentBeforeFailing(t *testing.T) {
	f := laterIntentFixture(t)
	f.s3.objects[f.dest("a.env")] = []byte("A=1\n")
	out := f.run()
	if out.State != "failed" || out.FailureShort != "AccessDenied on s3:PutObject" {
		t.Fatalf("outcome = %q %q: %s", out.State, out.FailureShort, out.FailureDetail)
	}
	r := result(t, out)
	want(t, "written", r.Written, uri(f, "a.env")...)
	want(t, "not written", r.NotWritten, uri(f, "b.env")...)
	if len(f.s3.puts) != 1 {
		t.Errorf("puts = %v; settling a.env must not write it", f.s3.puts)
	}
}

func TestEarlierFileFailureWithUncheckableLaterIntentIsUnknown(t *testing.T) {
	f := laterIntentFixture(t)
	aKey := f.dest("a.env")
	f.s3.getErr = func(key string) error {
		if key == aKey {
			return refusal(503, "SlowDown", "x")
		}
		return nil
	}
	out := f.run()
	if out.State != "unknown" || out.FailureShort != "could not check an interrupted write of a.env" {
		t.Fatalf("outcome = %q %q: %s", out.State, out.FailureShort, out.FailureDetail)
	}
	if !strings.Contains(out.FailureDetail, "AccessDenied on s3:PutObject") {
		t.Errorf("detail lost the original failure: %s", out.FailureDetail)
	}
	if len(f.s3.puts) != 1 {
		t.Errorf("puts = %v; a.env must not be written", f.s3.puts)
	}
}

// midLegCancelFixture writes a.env, then a cancel arrives; c.env's write was
// left intended by an earlier attempt.
func midLegCancelFixture(t *testing.T) *fixture {
	f := newFixture(t, file{"a.env", "a.env", "A=1\n"}, file{"b.env", "b.env", "B=1\n"}, file{"c.env", "c.env", "C=1\n"})
	f.env.PresetOps = []store.Op{{ID: 1, Name: "put-config", Key: "put-config:" + f.items[2].Destination, State: "intended"}}
	f.env.AfterDo = func(e *runnertest.Env) { e.SetCancel(true) }
	return f
}

func TestMidLegCancelSettlesInheritedIntentAndReconciles(t *testing.T) {
	f := midLegCancelFixture(t)
	f.s3.objects[f.dest("c.env")] = []byte("C=1\n")
	out := f.run()
	if out.State != "failed" || out.FailureShort != "cancelled; 1 files not written" {
		t.Fatalf("outcome = %q %q: %s", out.State, out.FailureShort, out.FailureDetail)
	}
	r := result(t, out)
	want(t, "written", r.Written, uri(f, "a.env", "c.env")...)
	want(t, "not written", r.NotWritten, uri(f, "b.env")...)
	if len(f.s3.puts) != 1 {
		t.Errorf("puts = %v; nothing may be written after the cancel", f.s3.puts)
	}
}

func TestMidLegCancelWithUncheckableInheritedIntentIsUnknown(t *testing.T) {
	f := midLegCancelFixture(t)
	cKey := f.dest("c.env")
	f.s3.getErr = func(key string) error {
		if key == cKey {
			return refusal(503, "SlowDown", "x")
		}
		return nil
	}
	out := f.run()
	if out.State != "unknown" || out.FailureShort != "could not check an interrupted write of c.env" {
		t.Fatalf("outcome = %q %q: %s", out.State, out.FailureShort, out.FailureDetail)
	}
	if len(f.s3.puts) != 1 {
		t.Errorf("puts = %v; nothing may be written after the cancel", f.s3.puts)
	}
}

func TestDoneOpButDestinationChangedIsWrittenAgain(t *testing.T) {
	f := newFixture(t, file{"web.env", "web.env", "A=1\n"})
	f.env.PresetOps = []store.Op{{ID: 1, Name: "put-config", Key: "put-config:" + f.items[0].Destination, State: "done"}}
	f.s3.objects[f.dest("web.env")] = []byte("OVERWRITTEN=1\n")
	out := f.run()
	if out.State != "succeeded" {
		t.Fatalf("outcome = %q %q: %s", out.State, out.FailureShort, out.FailureDetail)
	}
	want(t, "written", result(t, out).Written, uri(f, "web.env")...)
	if string(f.s3.objects[f.dest("web.env")]) != "A=1\n" || len(f.s3.puts) != 1 {
		t.Errorf("destination = %q, puts %v; the changed file must be written again", f.s3.objects[f.dest("web.env")], f.s3.puts)
	}
	if keys := f.env.DoKeys(); len(keys) != 2 || keys[1] != "put-config:"+f.items[0].Destination+":retry-2" {
		t.Errorf("Do keys = %v", keys)
	}
}
