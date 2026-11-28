// Package revalidate is the "revalidate" executor: once the run holds its
// locks it re-checks every target against the manifest the plan pinned. The
// service must still run the task definition it ran at planning time, the
// pinned image digest must still exist, every config destination must still
// hold what the plan saw there (or still be absent), and every stored copy
// must still hash to its recorded checksum.
//
// It only reads, so it needs no Env.Do. A mismatch fails the leg with the
// short reason "changed since planning", which the run engine turns into
// "invalidated". An AWS answer that is not a definite yes or no (access
// denied, throttling after retries, network) also fails the leg, but its short
// reason starts with "could not re-check the targets" and the run ends failed,
// not invalidated. Throttling, server errors and timeouts are retried first.
//
// Nothing here puts file contents or AWS error messages into a log line or an
// outcome: files are named with their name and the first 12 characters of a
// checksum, AWS failures with their error code and request id. Artifact keys
// are taken from the manifest and from nowhere else.
package revalidate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/engine"
	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner"
	"github.com/rcss-labs/yamdrake/internal/steps/resolve"
)

// DriftReason is the short failure reason of a leg whose targets changed.
const DriftReason = engine.RevalidateDriftReason

const couldNotCheckReason = engine.RevalidateCouldNotCheckReason

// attempts is how often one AWS call is tried when AWS answers with throttling,
// a server error or a timeout.
const attempts = 4

const defaultRetryBase = 500 * time.Millisecond

// ArtifactReader reads a stored copy and checks its SHA-256. *artifacts.Store
// satisfies it. Deps.Artifacts must also implement it (as *artifacts.Store
// does); an executor built without one fails every run that has stored copies.
type ArtifactReader interface {
	Get(ctx context.Context, key, wantSHA256 string) ([]byte, error)
}

type executor struct{ d resolve.Deps }

// New returns the "revalidate" executor.
func New(d resolve.Deps) runner.Executor {
	if d.RetryBase <= 0 {
		d.RetryBase = defaultRetryBase
	}
	if d.Clients == nil {
		options := d.Options
		d.Clients = func(cfg aws.Config) resolve.Clients { return resolve.NewClients(cfg, options) }
	}
	return &executor{d: d}
}

// finding is one thing that differs from the manifest (drift) or that could
// not be checked.
type finding struct {
	drift  bool
	detail string
}

type revalidation struct {
	x       *executor
	env     runner.Env
	run     queue.Claimed
	session string

	clients  map[string]resolve.Clients // by region
	findings []finding
}

// Execute checks the run's manifest against the world. See the package comment.
func (x *executor) Execute(ctx context.Context, env runner.Env, task queue.Claimed) queue.Outcome {
	run := task.Run
	if env.CancelRequested(ctx) {
		return queue.Outcome{State: "cancelled"}
	}
	if len(run.Manifest) == 0 {
		env.Log("error", "The run has no manifest to check against.")
		return failed("the run has no manifest", "revalidate needs the manifest the plan pinned")
	}
	var m manifest.Manifest
	if err := json.Unmarshal(run.Manifest, &m); err != nil || len(m.Services) == 0 {
		// The decoder's message can quote the document, so it is not used.
		env.Log("error", "The run's manifest could not be read.")
		return failed("the run's manifest could not be read", "it is not a manifest with services")
	}
	env.Log("info", fmt.Sprintf("Re-checking %d services of manifest %s against %s in account %s.", len(m.Services), shortID(m.ID), run.Environment, run.AWSAccount))

	r := &revalidation{
		x: x, env: env, run: task,
		session: fmt.Sprintf("yumdrake-runner-run-%d", run.ID),
		clients: map[string]resolve.Clients{},
	}
	for _, service := range m.Services {
		if ctx.Err() != nil {
			break
		}
		r.service(ctx, service)
	}

	if ctx.Err() != nil {
		return failed("re-checking was interrupted", ctx.Err().Error())
	}
	if env.CancelRequested(ctx) {
		return queue.Outcome{State: "cancelled"}
	}
	return r.outcome(len(m.Services))
}

func (r *revalidation) outcome(services int) queue.Outcome {
	if len(r.findings) == 0 {
		r.env.Log("ok", fmt.Sprintf("Nothing changed since planning in %d services.", services))
		return queue.Outcome{State: "succeeded"}
	}
	drift := false
	lines := make([]string, len(r.findings))
	for i, f := range r.findings {
		drift = drift || f.drift
		lines[i] = f.detail
	}
	reason := couldNotCheckReason + ": " + firstUnchecked(r.findings)
	if drift {
		reason = DriftReason
	}
	r.env.Log("error", fmt.Sprintf("%s: %d findings.", reason, len(r.findings)))
	return failed(reason, strings.Join(lines, "\n"))
}

// firstUnchecked is the first finding that is not drift, as the short reason
// of a leg that could not re-check: the service and the AWS code, no more.
func firstUnchecked(findings []finding) string {
	for _, f := range findings {
		if !f.drift {
			return f.detail
		}
	}
	return "unknown"
}

func failed(short, detail string) queue.Outcome {
	return queue.Outcome{State: "failed", FailureShort: short, FailureDetail: detail}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// shortSum is the first 12 characters of a checksum, the only part of it that
// is shown.
func shortSum(sum string) string {
	sum = strings.TrimPrefix(sum, "sha256:")
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// changed records that something differs from the manifest.
func (r *revalidation) changed(service, format string, args ...any) {
	detail := service + ": " + fmt.Sprintf(format, args...)
	r.findings = append(r.findings, finding{drift: true, detail: detail})
	r.env.Log("error", "["+service+"] "+detail[len(service)+2:])
}

// unchecked records an answer that could not be had.
func (r *revalidation) unchecked(service, detail string) {
	r.findings = append(r.findings, finding{detail: service + ": " + detail})
	r.env.Log("error", "["+service+"] "+detail)
}

// call runs one AWS call under the per-call deadline, repeating it while AWS
// answers with throttling, a server error or a timeout.
func (r *revalidation) call(ctx context.Context, fn func(ctx context.Context) error) error {
	return resolve.Retry(ctx, attempts, r.x.d.RetryBase, fn)
}

func (r *revalidation) ok(service, format string, args ...any) {
	r.env.Log("ok", "["+service+"] "+fmt.Sprintf(format, args...))
}

// clientsFor returns the clients of the run's account in region, asking for
// credentials once per region.
func (r *revalidation) clientsFor(ctx context.Context, region string) (resolve.Clients, error) {
	if c, ok := r.clients[region]; ok {
		return c, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, resolve.CallTimeout)
	defer cancel()
	cfg, err := r.x.d.AWS(callCtx, r.run.Run.AWSAccount, region, r.session)
	if err != nil {
		return resolve.Clients{}, err
	}
	c := r.x.d.Clients(cfg)
	r.clients[region] = c
	return c, nil
}

func (r *revalidation) service(ctx context.Context, s manifest.Service) {
	name := s.Name
	region := ""
	if ref, err := awsx.ParseECSService(s.ECSService); err == nil {
		region = ref.Region
		r.taskDefinition(ctx, s, ref)
	} else {
		r.unchecked(name, "the service identifier in the manifest is not usable")
	}
	if s.TargetTaskDefinition != "" {
		// A restore puts the service on a task definition that exists already:
		// that is what must still be usable, not an image found by digest.
		r.targetTaskDefinition(ctx, s, region)
	} else {
		r.image(ctx, s, &region)
	}
	for _, item := range s.Config {
		r.destination(ctx, s, item, region)
		r.storedCopy(ctx, name, item.Name, "new", item.ArtifactKey, item.SHA256)
		if item.PreviousExists {
			r.storedCopy(ctx, name, item.Name, "previous", item.PreviousArtifactKey, item.PreviousSHA256)
		}
	}
}

// --- ECS --------------------------------------------------------------------

func (r *revalidation) taskDefinition(ctx context.Context, s manifest.Service, ref awsx.ECSServiceRef) {
	clients, err := r.clientsFor(ctx, ref.Region)
	if err != nil {
		r.unchecked(s.Name, resolve.ErrorDetail("sign-in", err))
		return
	}
	var described *ecs.DescribeServicesOutput
	err = r.call(ctx, func(ctx context.Context) (callErr error) {
		described, callErr = clients.ECS.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster: aws.String(ref.Cluster), Services: []string{ref.Service},
		})
		return callErr
	})
	if err != nil {
		r.unchecked(s.Name, resolve.ErrorDetail("ecs:DescribeServices", err))
		return
	}
	// Only a service of the environment's account and region counts; a name
	// alone never matches.
	var candidates []ecstypes.Service
	for _, candidate := range described.Services {
		if problem := resolve.ARNScopeProblem(aws.ToString(candidate.ServiceArn), r.run.Run.AWSAccount, ref.Region); problem != "" {
			r.unchecked(s.Name, fmt.Sprintf("service %s was refused: %s", ref.Service, problem))
			return
		}
		candidates = append(candidates, candidate)
	}
	var found *struct{ status, taskDefinition string }
	for _, candidate := range candidates {
		if aws.ToString(candidate.ServiceArn) == s.ECSService || aws.ToString(candidate.ServiceName) == ref.Service {
			found = &struct{ status, taskDefinition string }{aws.ToString(candidate.Status), aws.ToString(candidate.TaskDefinition)}
			break
		}
	}
	// same rule as resolve: ECS returned only the service asked for
	if found == nil && len(candidates) == 1 {
		c := candidates[0]
		found = &struct{ status, taskDefinition string }{aws.ToString(c.Status), aws.ToString(c.TaskDefinition)}
	}
	switch {
	case found == nil:
		r.changed(s.Name, "service %s in cluster %s no longer exists; it ran %s at planning time", ref.Service, ref.Cluster, s.PreviousTaskDefinition)
	case found.status != "ACTIVE":
		r.changed(s.Name, "service %s is now %s, not ACTIVE; it ran %s at planning time", ref.Service, found.status, s.PreviousTaskDefinition)
	case found.taskDefinition != s.PreviousTaskDefinition:
		r.changed(s.Name, "service %s runs %s now; it ran %s at planning time", ref.Service, found.taskDefinition, s.PreviousTaskDefinition)
	default:
		r.ok(s.Name, "service still runs %s", s.PreviousTaskDefinition)
	}
}

// targetTaskDefinition checks that the task definition a restore goes back to
// is still registered and ACTIVE.
func (r *revalidation) targetTaskDefinition(ctx context.Context, s manifest.Service, region string) {
	if region == "" {
		r.unchecked(s.Name, "the region of the target task definition is unknown")
		return
	}
	clients, err := r.clientsFor(ctx, region)
	if err != nil {
		r.unchecked(s.Name, resolve.ErrorDetail("sign-in", err))
		return
	}
	var out *ecs.DescribeTaskDefinitionOutput
	err = r.call(ctx, func(ctx context.Context) (callErr error) {
		out, callErr = clients.ECS.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(s.TargetTaskDefinition)})
		return callErr
	})
	switch {
	case resolve.ErrorCode(err) == "ClientException": // what ECS answers for a task definition it does not know
		r.changed(s.Name, "task definition %s to restore no longer exists", s.TargetTaskDefinition)
	case err != nil:
		r.unchecked(s.Name, resolve.ErrorDetail("ecs:DescribeTaskDefinition", err))
	case out.TaskDefinition == nil || out.TaskDefinition.Status != ecstypes.TaskDefinitionStatusActive:
		status := "missing"
		if out.TaskDefinition != nil {
			status = string(out.TaskDefinition.Status)
		}
		r.changed(s.Name, "task definition %s to restore is now %s, not ACTIVE", s.TargetTaskDefinition, status)
	default:
		r.ok(s.Name, "task definition %s to restore is still ACTIVE", s.TargetTaskDefinition)
	}
}

// --- ECR --------------------------------------------------------------------

func (r *revalidation) image(ctx context.Context, s manifest.Service, region *string) {
	ref, err := awsx.ParseECRRepository(s.ECRRepository, r.x.d.Options)
	if err != nil {
		r.unchecked(s.Name, "the repository in the manifest is not usable")
		return
	}
	if *region == "" {
		*region = ref.Region
	}
	clients, err := r.clientsFor(ctx, ref.Region)
	if err != nil {
		r.unchecked(s.Name, resolve.ErrorDetail("sign-in", err))
		return
	}
	var out *ecr.DescribeImagesOutput
	err = r.call(ctx, func(ctx context.Context) (callErr error) {
		out, callErr = clients.ECR.DescribeImages(ctx, &ecr.DescribeImagesInput{
			RegistryId: aws.String(ref.Account), RepositoryName: aws.String(ref.Name),
			ImageIds: []ecrtypes.ImageIdentifier{{ImageDigest: aws.String(s.ImageDigest)}},
		})
		return callErr
	})
	if err != nil {
		var notFound *ecrtypes.ImageNotFoundException
		var noRepo *ecrtypes.RepositoryNotFoundException
		if errors.As(err, &notFound) || errors.As(err, &noRepo) {
			r.changed(s.Name, "image sha256:%s is no longer in repository %s", shortSum(s.ImageDigest), ref.Name)
			return
		}
		r.unchecked(s.Name, resolve.ErrorDetail("ecr:DescribeImages", err))
		return
	}
	for _, detail := range out.ImageDetails {
		if aws.ToString(detail.ImageDigest) == s.ImageDigest {
			r.ok(s.Name, "image sha256:%s still exists", shortSum(s.ImageDigest))
			return
		}
	}
	r.changed(s.Name, "image sha256:%s is no longer in repository %s", shortSum(s.ImageDigest), ref.Name)
}

// --- config destinations ------------------------------------------------------

func (r *revalidation) destination(ctx context.Context, s manifest.Service, item manifest.ConfigItem, region string) {
	ref, err := awsx.ParseS3(item.Destination)
	if err != nil {
		r.unchecked(s.Name, "the destination of "+item.Name+" in the manifest is not usable")
		return
	}
	if region == "" {
		r.unchecked(s.Name, "the region of "+item.Name+" is unknown")
		return
	}
	clients, err := r.clientsFor(ctx, region)
	if err != nil {
		r.unchecked(s.Name, resolve.ErrorDetail("sign-in", err))
		return
	}
	// The body is read inside the call: its context must outlive the read.
	var data []byte
	var readErr error
	err = r.call(ctx, func(ctx context.Context) error {
		data, readErr = nil, nil
		out, callErr := clients.S3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(ref.Bucket), Key: aws.String(ref.Key)})
		if callErr != nil {
			return callErr
		}
		defer func() { _ = out.Body.Close() }()
		data, readErr = io.ReadAll(io.LimitReader(out.Body, artifacts.MaxObjectBytes+1))
		return nil
	})
	missing := isMissing(err)
	if err != nil && !missing {
		r.unchecked(s.Name, resolve.ErrorDetail("s3:GetObject", err))
		return
	}

	switch {
	case missing && !item.PreviousExists:
		r.ok(s.Name, "%s: destination is still absent", item.Name)
	case missing:
		r.changed(s.Name, "%s: destination no longer exists; it held %s at planning time", item.Name, shortSum(item.PreviousSHA256))
	default:
		switch {
		case readErr != nil:
			r.unchecked(s.Name, "reading the destination of "+item.Name+" failed")
		case len(data) > artifacts.MaxObjectBytes:
			r.changed(s.Name, "%s: destination is now larger than %d bytes", item.Name, artifacts.MaxObjectBytes)
		case !item.PreviousExists:
			r.changed(s.Name, "%s: destination did not exist at planning time and now holds %s", item.Name, shortSum(sumOf(data)))
		case sumOf(data) != item.PreviousSHA256:
			r.changed(s.Name, "%s: destination held %s at planning time and now holds %s", item.Name, shortSum(item.PreviousSHA256), shortSum(sumOf(data)))
		default:
			r.ok(s.Name, "%s: destination still holds %s", item.Name, shortSum(item.PreviousSHA256))
		}
	}
}

func isMissing(err error) bool {
	if err == nil {
		return false
	}
	var noKey *s3types.NoSuchKey
	var notFound *s3types.NotFound
	if errors.As(err, &noKey) || errors.As(err, &notFound) {
		return true
	}
	return resolve.ErrorCode(err) == "NoSuchKey" || resolve.ErrorCode(err) == "NotFound"
}

// --- stored copies ------------------------------------------------------------

// storedCopy checks one artifact. The key is the manifest's own; which copy it
// is ("new" or "previous") only shows in the log.
func (r *revalidation) storedCopy(ctx context.Context, service, file, which, key, wantSHA256 string) {
	if wantSHA256 == "" && key == "" {
		return // nothing was stored for this item
	}
	if key == "" {
		r.changed(service, "%s: the manifest records no stored copy (%s) of the file", file, which)
		return
	}
	reader, ok := r.x.d.Artifacts.(ArtifactReader)
	if !ok {
		r.unchecked(service, "the artifact store cannot read stored copies")
		return
	}
	err := r.call(ctx, func(ctx context.Context) error {
		_, getErr := reader.Get(ctx, key, wantSHA256)
		return getErr
	})
	switch {
	case err == nil:
		r.ok(service, "%s: stored copy (%s) still hashes to %s", file, which, shortSum(wantSHA256))
	case errors.Is(err, artifacts.ErrCorrupt), errors.Is(err, artifacts.ErrTooLarge), isMissing(err):
		r.changed(service, "%s: stored copy (%s) no longer matches its checksum %s", file, which, shortSum(wantSHA256))
	default:
		r.unchecked(service, resolve.ErrorDetail("reading the stored copy of "+file, err))
	}
}

func sumOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
