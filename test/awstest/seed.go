package awstest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	ecrtypes "github.com/aws/aws-sdk-go-v2/service/ecr/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3Region is where Bucket, PutObject and Object work: they take no region.
const s3Region = "us-east-1"

func (a *Account) s3Client() *s3.Client {
	return s3.NewFromConfig(a.AWSConfig(s3Region), func(o *s3.Options) { o.UsePathStyle = true })
}

// Bucket creates a bucket, with versioning on when versioned is true.
func (a *Account) Bucket(name string, versioned bool) {
	a.w.t.Helper()
	ctx := context.Background()
	client := a.s3Client()
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
		a.w.t.Fatalf("awstest: CreateBucket %s: %v", name, err)
	}
	if !versioned {
		return
	}
	_, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(name),
		VersioningConfiguration: &s3types.VersioningConfiguration{Status: s3types.BucketVersioningStatusEnabled},
	})
	if err != nil {
		a.w.t.Fatalf("awstest: enable versioning on %s: %v", name, err)
	}
}

// PutObject writes an object.
func (a *Account) PutObject(bucket, key string, body []byte) {
	a.w.t.Helper()
	_, err := a.s3Client().PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body),
	})
	if err != nil {
		a.w.t.Fatalf("awstest: PutObject %s/%s: %v", bucket, key, err)
	}
}

// Object reads an object. The bool is false when the key does not exist.
func (a *Account) Object(bucket, key string) ([]byte, bool) {
	a.w.t.Helper()
	out, err := a.s3Client().GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		var noKey *s3types.NoSuchKey
		if errors.As(err, &noKey) {
			return nil, false
		}
		a.w.t.Fatalf("awstest: GetObject %s/%s: %v", bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		a.w.t.Fatalf("awstest: read %s/%s: %v", bucket, key, err)
	}
	return data, true
}

// KMSKey creates a symmetric key and returns its id.
func (a *Account) KMSKey(region string) (keyID string) {
	a.w.t.Helper()
	out, err := kms.NewFromConfig(a.AWSConfig(region)).CreateKey(context.Background(), &kms.CreateKeyInput{})
	if err != nil || out.KeyMetadata == nil {
		a.w.t.Fatalf("awstest: CreateKey in %s: %v", region, err)
	}
	return aws.ToString(out.KeyMetadata.KeyId)
}

// imageDir is test/awstest/image, found relative to this source file.
func imageDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "image")
}

// localImage builds the test image once per marker in this World and returns
// its image id. The marker is written into the image, so different markers
// give different digests.
//
// The image is held by a local tag unique to this World, and callers push by
// id. A name shared with other tests or processes can be rebuilt to a different
// image id between two pushes of one Repo call, and the old image deleted.
func (w *World) localImage(marker string) string { return w.buildImage(marker, "") }

// buildImage is localImage for an image that exits with exitCode right away
// when exitCode is not empty. The cache is keyed by marker and exit code.
func (w *World) buildImage(marker, exitCode string) string {
	w.t.Helper()
	w.imagesMu.Lock()
	defer w.imagesMu.Unlock()
	cacheKey := marker + "\x00" + exitCode
	if id, ok := w.images[cacheKey]; ok {
		return id
	}
	if w.images == nil {
		w.images = map[string]string{}
	}
	hold := "yumdrake-awstest:hold-" + RandomSuffix() + RandomSuffix()
	id := w.dockerRun("build", "-q", "--build-arg", "MARKER="+marker, "--build-arg", "EXIT_CODE="+exitCode, "-t", hold, imageDir())
	w.t.Cleanup(func() { _ = exec.Command("docker", "rmi", hold).Run() }) //nolint:gosec // test helper: arguments are ours
	w.images[cacheKey] = id
	return id
}

// registryHost is the host:port to docker push to. Floci serves its registry
// on the same port as its API, as <host>/<account>/<region>/<repository>.
func (w *World) registryHost() string {
	u, err := url.Parse(w.Endpoint)
	if err != nil {
		w.t.Fatalf("awstest: endpoint: %v", err)
	}
	return u.Host
}

func (a *Account) push(region, repo, tag, marker string) {
	a.w.t.Helper()
	a.pushImage(region, repo, tag, a.w.localImage(marker))
}

func (a *Account) pushImage(region, repo, tag, imageID string) {
	a.w.t.Helper()
	remote := fmt.Sprintf("%s/%s/%s/%s:%s", a.w.registryHost(), a.ID, region, repo, tag)
	a.w.dockerRun("tag", imageID, remote)
	a.w.t.Cleanup(func() { _ = exec.Command("docker", "rmi", remote).Run() }) //nolint:gosec // test helper: arguments are ours
	a.w.dockerRun("push", remote)
}

func (a *Account) digestOf(region, repo, tag string) string {
	a.w.t.Helper()
	out, err := ecr.NewFromConfig(a.AWSConfig(region)).DescribeImages(context.Background(), &ecr.DescribeImagesInput{
		RepositoryName: aws.String(repo),
	})
	if err != nil {
		a.w.t.Fatalf("awstest: DescribeImages %s: %v", repo, err)
	}
	for _, d := range out.ImageDetails {
		for _, got := range d.ImageTags {
			if got == tag {
				return aws.ToString(d.ImageDigest)
			}
		}
	}
	a.w.t.Fatalf("awstest: image %s:%s not found after push", repo, tag)
	return ""
}

// Repo creates a repository and pushes the one test image under every tag, so
// all tags share one digest. It returns the repository URI (no tag) and the digest.
func (a *Account) Repo(region, name string, tags ...string) (uri string, digest string) {
	a.w.t.Helper()
	if len(tags) == 0 {
		a.w.t.Fatalf("awstest: Repo(%s) needs at least one tag", name)
	}
	out, err := ecr.NewFromConfig(a.AWSConfig(region)).CreateRepository(context.Background(), &ecr.CreateRepositoryInput{
		RepositoryName:     aws.String(name),
		ImageTagMutability: ecrtypes.ImageTagMutabilityMutable,
	})
	if err != nil || out.Repository == nil {
		a.w.t.Fatalf("awstest: CreateRepository %s: %v", name, err)
	}
	for _, tag := range tags {
		a.push(region, name, tag, "base")
	}
	return aws.ToString(out.Repository.RepositoryUri), a.digestOf(region, name, tags[0])
}

// RepoImage pushes an image with different content under one tag of an
// existing repository, so the tag moves to a new digest.
func (a *Account) RepoImage(region, name, tag, marker string) (digest string) {
	a.w.t.Helper()
	a.push(region, name, tag, marker)
	return a.digestOf(region, name, tag)
}

// RepoCrashingImage pushes an image that exits with exitCode as soon as it
// starts under one tag of an existing repository.
func (a *Account) RepoCrashingImage(region, name, tag, marker string, exitCode int) (digest string) {
	a.w.t.Helper()
	a.pushImage(region, name, tag, a.w.buildImage(marker, strconv.Itoa(exitCode)))
	return a.digestOf(region, name, tag)
}

// Service creates a cluster, a task definition (family = name) and a service
// of one task running image, and waits until the task is running.
func (a *Account) Service(region, cluster, name, image string) (serviceARN, taskDefARN string) {
	a.w.t.Helper()
	ctx := context.Background()
	client := ecs.NewFromConfig(a.AWSConfig(region))
	subnet := a.subnet(region)

	if _, err := client.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: aws.String(cluster)}); err != nil {
		a.w.t.Fatalf("awstest: CreateCluster %s: %v", cluster, err)
	}
	// Cleanup runs even when a later step fails. The containers of the tasks
	// this call caused are found through the cluster and removed by task id.
	a.w.t.Cleanup(func() { a.cleanupCluster(client, cluster, name) })

	td, err := client.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family:                  aws.String(name),
		NetworkMode:             ecstypes.NetworkModeAwsvpc,
		RequiresCompatibilities: []ecstypes.Compatibility{ecstypes.CompatibilityFargate},
		Cpu:                     aws.String("256"),
		Memory:                  aws.String("512"),
		ContainerDefinitions: []ecstypes.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String(image), Essential: aws.Bool(true),
		}},
	})
	if err != nil || td.TaskDefinition == nil {
		a.w.t.Fatalf("awstest: RegisterTaskDefinition %s: %v", name, err)
	}
	svc, err := client.CreateService(ctx, &ecs.CreateServiceInput{
		Cluster:        aws.String(cluster),
		ServiceName:    aws.String(name),
		TaskDefinition: td.TaskDefinition.TaskDefinitionArn,
		DesiredCount:   aws.Int32(1),
		LaunchType:     ecstypes.LaunchTypeFargate,
		NetworkConfiguration: &ecstypes.NetworkConfiguration{AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
			Subnets: []string{subnet},
		}},
	})
	if err != nil || svc.Service == nil {
		a.w.t.Fatalf("awstest: CreateService %s: %v", name, err)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := client.DescribeServices(ctx, &ecs.DescribeServicesInput{Cluster: aws.String(cluster), Services: []string{name}})
		if err == nil && len(out.Services) == 1 {
			s := out.Services[0]
			if s.RunningCount >= 1 && len(s.Deployments) > 0 && s.Deployments[0].RolloutState == ecstypes.DeploymentRolloutStateCompleted {
				return aws.ToString(s.ServiceArn), aws.ToString(td.TaskDefinition.TaskDefinitionArn)
			}
		}
		time.Sleep(time.Second)
	}
	a.w.t.Fatalf("awstest: service %s/%s did not reach running within 2 minutes", cluster, name)
	return "", ""
}

// cleanupCluster deletes the service and cluster and removes the sibling
// containers of exactly the tasks that existed in this cluster. Deleting the
// service already removes them in Floci 2.5.0; the explicit removal covers
// tasks that were stopped but whose containers linger. Containers of other
// tests, other Flocis and the shared ECR registry are never matched.
func (a *Account) cleanupCluster(client *ecs.Client, cluster, service string) {
	ctx := context.Background()
	var taskIDs []string
	for _, status := range []ecstypes.DesiredStatus{ecstypes.DesiredStatusRunning, ecstypes.DesiredStatusStopped} {
		out, err := client.ListTasks(ctx, &ecs.ListTasksInput{Cluster: aws.String(cluster), DesiredStatus: status})
		if err != nil {
			continue
		}
		for _, arn := range out.TaskArns {
			taskIDs = append(taskIDs, arn[strings.LastIndex(arn, "/")+1:])
		}
	}
	_, _ = client.DeleteService(ctx, &ecs.DeleteServiceInput{Cluster: aws.String(cluster), Service: aws.String(service), Force: aws.Bool(true)})
	_, _ = client.DeleteCluster(ctx, &ecs.DeleteClusterInput{Cluster: aws.String(cluster)})
	for _, id := range taskIDs {
		removeContainers(containerIDs("name=^floci-aws-ecs-" + id))
	}
	// Containers created for this test's own random account that outlived the
	// service (a crash-looping task can leave more than one).
	if _, ours := randomAccounts.Load(a.ID); ours {
		removeContainers(containerIDs("label=io.floci.service=ecs", "label=io.floci.account="+a.ID))
	}
}
