// Package copyconfig is the "copy-config" executor: it writes the config files
// a manifest pinned for one service to their S3 destinations, using the
// deployment credentials of the run's account.
//
// Every file is read first. One that already holds the pinned content is left
// alone; one that differs is written from the stored copy (checksum verified)
// through Env.Do and read back to confirm the hash. What the leg did is always
// reported per file, by destination, in the outcome's result: written, already
// in place, not written, or not restored.
//
// Writes are classified for the run engine's reconciliation: an HTTP 4xx
// answer to PutObject is a refusal (the leg fails, locks are released); no
// answer, a 5xx, a failed read-back or a read-back that differs may mean the
// write happened, so the operation stays intended and the leg is "unknown".
//
// Nothing here puts file contents or AWS error messages into a log line or an
// outcome: files are named by name, destination, size and the first 12
// characters of their checksum; AWS failures by error code and request id.
package copyconfig

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

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner"
	"github.com/rcss-labs/yamdrake/internal/steps/resolve"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// S3API is the part of the S3 client the step uses. *s3.Client satisfies it.
type S3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// ArtifactReader reads a stored copy and checks its SHA-256. *artifacts.Store
// satisfies it.
type ArtifactReader interface {
	Get(ctx context.Context, key, wantSHA256 string) ([]byte, error)
}

// Deps are what the executor needs from the outside.
type Deps struct {
	// AWS returns an SDK configuration with the deployment credentials of an
	// account (the runner's role-assumption). session names the assumed role
	// session.
	AWS func(ctx context.Context, accountID, region, session string) (aws.Config, error)
	// S3 builds the S3 client. nil means s3.NewFromConfig, path-style when
	// Options points the SDK at an emulator.
	S3        func(cfg aws.Config) S3API
	Artifacts ArtifactReader
	Options   awsx.Options
}

// Result is the leg's result in every outcome: each config item of the
// service, by destination URI, in exactly one list.
type Result struct {
	Written        []string `json:"written"`
	AlreadyInPlace []string `json:"already_in_place"`
	NotWritten     []string `json:"not_written"`
	NotRestored    []string `json:"not_restored"`
}

const (
	opName       = "put-config"
	nothingToDo  = "no env files or TOMLs for this service, nothing to copy"
	shortSumSize = 12
)

type executor struct{ d Deps }

// New returns the "copy-config" executor.
func New(d Deps) runner.Executor {
	if d.S3 == nil {
		options := d.Options
		d.S3 = func(cfg aws.Config) S3API {
			return s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = options.UsePathStyleS3() })
		}
	}
	return &executor{d: d}
}

// leg is one execution of the step.
type leg struct {
	x       *executor
	ctx     context.Context
	env     runner.Env
	task    queue.Claimed
	service manifest.Service
	items   []manifest.ConfigItem
	region  string
	client  S3API
	ops     []store.Op

	res   Result
	names map[string]string // destination URI -> file name
}

// Execute copies the service's config files. See the package comment.
func (x *executor) Execute(ctx context.Context, env runner.Env, task queue.Claimed) queue.Outcome {
	r := &leg{x: x, ctx: ctx, env: env, task: task, names: map[string]string{}}

	var m manifest.Manifest
	found := json.Unmarshal(task.Run.Manifest, &m) == nil
	if found {
		found = false
		for _, s := range m.Services {
			if s.Name == task.Leg.Service {
				r.service, found = s, true
				break
			}
		}
	}
	if !found {
		env.Log("error", "The run's manifest has no entry for this service.")
		return r.end("failed", "the run's manifest has no entry for this service", "")
	}
	r.items = r.service.Config
	for _, item := range r.items {
		r.names[item.Destination] = item.Name
	}
	if len(r.items) == 0 {
		env.Log("info", nothingToDo)
		return r.end("succeeded", "", "")
	}
	ref, err := awsx.ParseECSService(r.service.ECSService)
	if err != nil {
		env.Log("error", "The service identifier in the manifest is not usable.")
		return r.stop(0, "failed", "the service identifier in the manifest is not usable", "")
	}
	r.region = ref.Region

	ops, err := env.Ops(ctx)
	if err != nil {
		env.Log("error", "The leg's earlier operations could not be read.")
		return r.stop(0, "unknown", "could not read the leg's earlier operations", resolve.ErrorDetail("reading operations", err))
	}
	r.ops = ops

	// A cancel that arrives while an earlier attempt's write is unresolved is
	// not honoured by stopping: the unresolved write is reconciled and nothing
	// new is written.
	reconcile := env.CancelRequested(ctx) && r.hasIntendedOp()

	for i, item := range r.items {
		if out := r.item(i, item, reconcile); out != nil {
			return *out
		}
	}
	if reconcile {
		if n := len(r.res.NotWritten); n > 0 {
			return r.end("failed", fmt.Sprintf("cancelled; %d files not written", n), r.listing())
		}
	}
	return r.end("succeeded", "", "")
}

// item handles one config item; a non-nil result ends the leg.
func (r *leg) item(i int, item manifest.ConfigItem, reconcile bool) *queue.Outcome {
	ctx, env := r.ctx, r.env
	if item.NotRestored != "" {
		r.res.NotRestored = append(r.res.NotRestored, item.Destination)
		env.Log("warn", fmt.Sprintf("%s: not restored: %s", item.Name, item.NotRestored))
		return nil
	}
	if r.client == nil {
		cfg, err := r.x.d.AWS(ctx, r.task.Run.AWSAccount, r.region, fmt.Sprintf("yumdrake-runner-run-%d", r.task.Run.ID))
		if err != nil {
			env.Log("error", fmt.Sprintf("Could not get credentials for account %s.", r.task.Run.AWSAccount))
			state := "failed"
			if r.hasIntendedOp() {
				state = "unknown" // an earlier write is still unresolved: nothing says it did not happen
			}
			return r.stopped(i, state, "could not get credentials for account "+r.task.Run.AWSAccount, resolve.ErrorDetail("sign-in", err))
		}
		r.client = r.x.d.S3(cfg)
	}

	current, missing, err := r.read(item.Destination)
	if err != nil {
		return r.readFailed(i, item, err, reconcile)
	}
	if !missing && sumOf(current) == item.SHA256 {
		if r.intendedFor(item.Destination) {
			r.res.Written = append(r.res.Written, item.Destination)
			env.Log("ok", fmt.Sprintf("%s: an earlier attempt's write is in place (%s)", item.Name, shortSum(item.SHA256)))
		} else {
			r.res.AlreadyInPlace = append(r.res.AlreadyInPlace, item.Destination)
			env.Log("info", fmt.Sprintf("%s: already in place (%s)", item.Name, shortSum(item.SHA256)))
		}
		return nil
	}

	if reconcile {
		r.res.NotWritten = append(r.res.NotWritten, item.Destination)
		env.Log("warn", fmt.Sprintf("%s: not written, the run was cancelled", item.Name))
		return nil
	}
	if env.CancelRequested(ctx) {
		env.Log("warn", fmt.Sprintf("Cancelled after %d of %d files.", i, len(r.items)))
		return r.stopped(i, "cancelled", fmt.Sprintf("cancelled after %d of %d files", i, len(r.items)), "")
	}

	data, err := r.storedCopy(item)
	if err != nil {
		env.Log("error", fmt.Sprintf("%s: the stored copy is not usable.", item.Name))
		if r.intendedFor(item.Destination) {
			return r.stopped(i, "unknown", "could not check an interrupted write of "+item.Name, resolve.ErrorDetail("reading the stored copy of "+item.Name, err))
		}
		return r.stopped(i, "failed", fmt.Sprintf("stored copy of %s is not usable", item.Name), resolve.ErrorDetail("reading the stored copy of "+item.Name, err))
	}
	return r.write(i, item, data)
}

func (r *leg) storedCopy(item manifest.ConfigItem) ([]byte, error) {
	if r.x.d.Artifacts == nil {
		return nil, errors.New("no artifact store")
	}
	return r.x.d.Artifacts.Get(r.ctx, item.ArtifactKey, item.SHA256)
}

// --- writing ----------------------------------------------------------------

// attempt is one call of Env.Do for a file.
type attempt struct {
	err    error // from Do
	fnRan  bool
	fnErr  error // what fn returned, nil on success
	refuse bool  // fnErr is a definite refusal
}

func (r *leg) write(i int, item manifest.ConfigItem, data []byte) *queue.Outcome {
	base := opName + ":" + item.Destination
	a := r.put(item, data, base)
	if errors.Is(a.err, runner.ErrAmbiguous) {
		// An earlier attempt intended this write and never recorded the result.
		current, missing, err := r.read(item.Destination)
		if err != nil {
			r.env.Log("error", fmt.Sprintf("%s: an interrupted write could not be checked.", item.Name))
			return r.stopped(i, "unknown", "could not check an interrupted write of "+item.Name, resolve.ErrorDetail("s3:GetObject", err))
		}
		if !missing && sumOf(current) == item.SHA256 {
			r.res.Written = append(r.res.Written, item.Destination)
			r.env.Log("ok", fmt.Sprintf("%s: an earlier attempt's write is in place (%s)", item.Name, shortSum(item.SHA256)))
			return nil
		}
		a = r.put(item, data, fmt.Sprintf("%s:retry-%d", base, r.task.Leg.Attempt))
	} else if a.err == nil && !a.fnRan {
		// An op recorded as done, yet the destination was just read and
		// differs: someone changed it after our write. Write it again.
		a = r.put(item, data, fmt.Sprintf("%s:retry-%d", base, r.task.Leg.Attempt))
	}

	switch {
	case a.err == nil:
		r.res.Written = append(r.res.Written, item.Destination)
		r.env.Log("ok", fmt.Sprintf("%s: written to %s (%d bytes, sha256 %s)", item.Name, item.Destination, len(data), shortSum(item.SHA256)))
		return nil
	case errors.Is(a.err, runner.ErrPreviouslyFailed):
		// By design (ruling R21) a refused write is not retried here: this only
		// happens when a runner died after recording the refusal. Finishing the
		// copy is a re-run of the failed run (new legs, no ops).
		current, missing, err := r.read(item.Destination)
		if err != nil {
			r.env.Log("error", fmt.Sprintf("%s: an earlier refused write could not be checked.", item.Name))
			return r.stopped(i, "unknown", "could not check an interrupted write of "+item.Name, resolve.ErrorDetail("s3:GetObject", err))
		}
		if !missing && sumOf(current) == item.SHA256 {
			r.res.AlreadyInPlace = append(r.res.AlreadyInPlace, item.Destination)
			r.env.Log("info", fmt.Sprintf("%s: already in place (%s)", item.Name, shortSum(item.SHA256)))
			return nil
		}
		r.env.Log("error", fmt.Sprintf("%s: an earlier attempt's write was refused and is not repeated.", item.Name))
		return r.stopped(i, "failed", fmt.Sprintf("an earlier attempt's s3:PutObject of %s was refused", item.Name), "")
	case a.refuse:
		code := resolve.ErrorCode(a.fnErr)
		if code == "" {
			code = "RequestFailed"
		}
		r.env.Log("error", fmt.Sprintf("%s: %s was refused (%s) and not written.", item.Name, item.Destination, code))
		return r.stopped(i, "failed", code+" on s3:PutObject", resolve.ErrorDetail("s3:PutObject", a.fnErr))
	case !a.fnRan:
		r.env.Log("error", fmt.Sprintf("%s: the write could not be recorded.", item.Name))
		return r.stopped(i, "unknown", "could not record the write of "+item.Name, resolve.ErrorDetail("recording the write", a.err))
	default:
		r.env.Log("error", fmt.Sprintf("%s: it is not known whether %s was written.", item.Name, item.Destination))
		return r.stopped(i, "unknown", "could not confirm the write of "+item.Name, resolve.ErrorDetail("s3:PutObject", a.err))
	}
}

// put writes one file through Env.Do under key.
func (r *leg) put(item manifest.ConfigItem, data []byte, key string) attempt {
	ref, err := awsx.ParseS3(item.Destination)
	if err != nil {
		return attempt{err: errors.New("the destination in the manifest is not usable"), fnRan: true, fnErr: err, refuse: true}
	}
	var a attempt
	intent := map[string]any{"destination": item.Destination, "sha256": item.SHA256, "size": len(data)}
	_, a.err = r.env.Do(r.ctx, opName, key, intent, func() (any, error) {
		a.fnRan = true
		_, err := r.client.PutObject(r.ctx, &s3.PutObjectInput{
			Bucket: aws.String(ref.Bucket), Key: aws.String(ref.Key), Body: bytes.NewReader(data),
		})
		if err != nil {
			if isRefusal(err) {
				a.fnErr, a.refuse = err, true
				return nil, err
			}
			a.fnErr = fmt.Errorf("%w: put gave no answer: %w", runner.ErrMaybeApplied, err)
			return nil, a.fnErr
		}
		back, missing, err := r.read(item.Destination)
		if err != nil || missing || sumOf(back) != item.SHA256 {
			// The put was accepted; whatever is there now cannot be trusted
			// to be ours, and only a person can say what to do.
			a.fnErr = fmt.Errorf("%w: the read-back did not confirm the write", runner.ErrMaybeApplied)
			return nil, a.fnErr
		}
		return map[string]string{"sha256": sumOf(back)}, nil
	})
	if a.refuse && r.ctx.Err() != nil {
		a.refuse = false
	}
	return a
}

// isRefusal reports whether err is an HTTP 4xx answer: the service looked at
// the request and said no. Anything else (no answer, 5xx, a request timeout)
// may have taken effect.
func isRefusal(err error) bool {
	var response *awshttp.ResponseError
	if !errors.As(err, &response) {
		return false
	}
	status := response.HTTPStatusCode()
	return status >= 400 && status < 500 && status != http.StatusRequestTimeout
}

// --- reading ----------------------------------------------------------------

// read returns what the destination holds; missing is true when it holds
// nothing. A failure other than "not there" is err.
func (r *leg) read(destination string) (data []byte, missing bool, err error) {
	ref, err := awsx.ParseS3(destination)
	if err != nil {
		return nil, false, err
	}
	out, err := r.client.GetObject(r.ctx, &s3.GetObjectInput{Bucket: aws.String(ref.Bucket), Key: aws.String(ref.Key)})
	if err != nil {
		if isMissing(err) {
			return nil, true, nil
		}
		return nil, false, err
	}
	defer func() { _ = out.Body.Close() }()
	data, err = io.ReadAll(io.LimitReader(out.Body, artifacts.MaxObjectBytes+1))
	if err != nil {
		return nil, false, err
	}
	return data, false, nil
}

func (r *leg) readFailed(i int, item manifest.ConfigItem, err error, reconcile bool) *queue.Outcome {
	code := resolve.ErrorCode(err)
	if code == "" {
		code = "RequestFailed"
	}
	detail := resolve.ErrorDetail("s3:GetObject", err)
	r.env.Log("error", fmt.Sprintf("%s: %s could not be read (%s).", item.Name, item.Destination, code))
	if r.intendedFor(item.Destination) {
		return r.stopped(i, "unknown", "could not check an interrupted write of "+item.Name, detail)
	}
	if reconcile || r.ctx.Err() != nil {
		return r.stopped(i, "unknown", "could not check "+item.Name, detail)
	}
	return r.stopped(i, "failed", code+" on s3:GetObject", detail)
}

func isMissing(err error) bool {
	var noKey *s3types.NoSuchKey
	var notFound *s3types.NotFound
	if errors.As(err, &noKey) || errors.As(err, &notFound) {
		return true
	}
	code := resolve.ErrorCode(err)
	return code == "NoSuchKey" || code == "NotFound"
}

// --- operations ---------------------------------------------------------------

func (r *leg) hasIntendedOp() bool {
	for _, op := range r.ops {
		if op.State == "intended" {
			return true
		}
	}
	return false
}

// intendedFor reports whether an earlier attempt intended a write to the
// destination and never recorded its result.
func (r *leg) intendedFor(destination string) bool {
	base := opName + ":" + destination
	for _, op := range r.ops {
		if op.State == "intended" && (op.Key == base || strings.HasPrefix(op.Key, base+":retry-")) {
			return true
		}
	}
	return false
}

// openIntentFrom reports whether an item at index i or later has a write an
// earlier attempt intended and never recorded.
func (r *leg) openIntentFrom(i int) bool {
	for _, item := range r.items[i:] {
		if item.NotRestored == "" && r.intendedFor(item.Destination) {
			return true
		}
	}
	return false
}

// --- ending the leg -------------------------------------------------------------

// stopped ends the leg at item i: it and every later item are put in a list.
func (r *leg) stopped(i int, state, short, detail string) *queue.Outcome {
	out := r.stop(i, state, short, detail)
	return &out
}

// stop is stopped without the pointer.
//
// A leg may end "failed" (locks released) only when none of its writes is
// still open, so before failing it settles every item left whose write an
// earlier attempt intended, by reading the destination: the pinned bytes mean
// Written, anything else NotWritten. Nothing new is written. If such a read
// fails, the leg ends "unknown" instead.
//
// The same holds for a cancel noticed mid-leg: with such a write still open it
// is settled first and the leg ends as in reconcile mode, "succeeded" when
// every file is in place and otherwise "failed", never "cancelled".
func (r *leg) stop(i int, state, short, detail string) queue.Outcome {
	settleCancel := state == "cancelled" && r.openIntentFrom(i)
	for _, item := range r.items[i:] {
		switch {
		case item.NotRestored != "":
			r.res.NotRestored = append(r.res.NotRestored, item.Destination)
		case (state == "failed" || state == "cancelled" && settleCancel) && r.intendedFor(item.Destination):
			if err := r.settle(item); err != nil {
				r.env.Log("error", fmt.Sprintf("%s: an interrupted write could not be checked.", item.Name))
				detail = joinLines(short, detail, resolve.ErrorDetail("s3:GetObject", err))
				state, short = "unknown", "could not check an interrupted write of "+item.Name
				r.res.NotWritten = append(r.res.NotWritten, item.Destination)
			}
		default:
			r.res.NotWritten = append(r.res.NotWritten, item.Destination)
		}
	}
	if settleCancel && state == "cancelled" {
		n := len(r.res.NotWritten)
		if n == 0 {
			return r.end("succeeded", "", "")
		}
		state, short, detail = "failed", fmt.Sprintf("cancelled; %d files not written", n), ""
	}
	if list := r.listing(); list != "" {
		if detail != "" {
			detail += "\n"
		}
		detail += list
	}
	return r.end(state, short, detail)
}

// settle reads the destination of an item whose write is still intended and
// files it under Written or NotWritten. It writes nothing.
func (r *leg) settle(item manifest.ConfigItem) error {
	if r.client == nil {
		return errors.New("no S3 client")
	}
	current, missing, err := r.read(item.Destination)
	if err != nil {
		return err
	}
	if !missing && sumOf(current) == item.SHA256 {
		r.res.Written = append(r.res.Written, item.Destination)
		r.env.Log("ok", fmt.Sprintf("%s: an earlier attempt's write is in place (%s)", item.Name, shortSum(item.SHA256)))
	} else {
		r.res.NotWritten = append(r.res.NotWritten, item.Destination)
	}
	return nil
}

func joinLines(lines ...string) string {
	var kept []string
	for _, l := range lines {
		if l != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// listing says in plain words where every file stands.
func (r *leg) listing() string {
	var parts []string
	for _, group := range []struct {
		label string
		uris  []string
	}{
		{"Written", r.res.Written}, {"Already in place", r.res.AlreadyInPlace},
		{"Not written", r.res.NotWritten}, {"Not restored", r.res.NotRestored},
	} {
		if len(group.uris) > 0 {
			parts = append(parts, group.label+": "+r.nameList(group.uris)+".")
		}
	}
	return strings.Join(parts, " ")
}

func (r *leg) nameList(uris []string) string {
	names := make([]string, len(uris))
	for i, uri := range uris {
		names[i] = r.names[uri]
	}
	return strings.Join(names, ", ")
}

// end logs the summary and builds the outcome.
func (r *leg) end(state, short, detail string) queue.Outcome {
	if len(r.items) > 0 {
		r.env.Log("info", fmt.Sprintf("Wrote %d, already in place %d, not written %d, not restored %d.",
			len(r.res.Written), len(r.res.AlreadyInPlace), len(r.res.NotWritten), len(r.res.NotRestored)))
		if len(r.res.NotWritten) > 0 {
			level := "error"
			if state == "cancelled" {
				level = "warn"
			}
			r.env.Log(level, "Not written: "+r.nameList(r.res.NotWritten)+".")
		}
	}
	for _, list := range []*[]string{&r.res.Written, &r.res.AlreadyInPlace, &r.res.NotWritten, &r.res.NotRestored} {
		if *list == nil {
			*list = []string{}
		}
	}
	result, _ := json.Marshal(r.res)
	return queue.Outcome{State: state, FailureShort: short, FailureDetail: detail, Result: result}
}

func sumOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func shortSum(sum string) string {
	sum = strings.TrimPrefix(sum, "sha256:")
	if len(sum) > shortSumSize {
		return sum[:shortSumSize]
	}
	return sum
}
