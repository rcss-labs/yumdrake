// Package ecsdeploy builds the ECS task definition a deployment registers.
package ecsdeploy

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// Tag keys that identify which deployment registered a task definition.
const (
	TagVersion  = "yumdrake:version"
	TagRun      = "yumdrake:run"
	TagLeg      = "yumdrake:leg"
	TagManifest = "yumdrake:manifest"
)

// ErrNoMatchingContainer means no container in the previous definition uses the service's repository.
var ErrNoMatchingContainer = errors.New("no container uses the service's repository")

// Stamp identifies the deployment that registers a task definition.
type Stamp struct {
	Version    string
	ManifestID string
	RunID      int64
	LegID      int64
}

// ImageRepository returns the image reference without its tag or digest.
func ImageRepository(image string) string {
	if i := strings.IndexByte(image, '@'); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndexByte(image, ':'); i > strings.LastIndexByte(image, '/') {
		image = image[:i]
	}
	return image
}

// NextTaskDefinition copies prev, points the containers that use repositoryURI at
// repositoryURI@digest, and merges the stamp tags into prevTags. Other containers and
// every other field are carried over unchanged. prev is never modified.
func NextTaskDefinition(prev ecstypes.TaskDefinition, prevTags []ecstypes.Tag, repositoryURI string, digest string, s Stamp) (*ecs.RegisterTaskDefinitionInput, error) {
	repository := strings.TrimSuffix(repositoryURI, "/")
	if repository == "" {
		return nil, errors.New("repository URI is empty")
	}
	if !strings.HasPrefix(digest, "sha256:") {
		return nil, fmt.Errorf("digest %q is not a sha256 digest", digest)
	}

	containers := slices.Clone(prev.ContainerDefinitions)
	var images []string
	matched := false
	for i := range containers {
		image := aws.ToString(containers[i].Image)
		images = append(images, image)
		if ImageRepository(image) == repository {
			containers[i].Image = aws.String(repository + "@" + digest)
			matched = true
		}
	}
	if !matched {
		return nil, fmt.Errorf("%w: repository %s; images found: %s", ErrNoMatchingContainer, repositoryURI, strings.Join(images, ", "))
	}

	return &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions:          containers,
		Family:                        prev.Family,
		Cpu:                           prev.Cpu,
		EnableFaultInjection:          prev.EnableFaultInjection,
		EphemeralStorage:              prev.EphemeralStorage,
		ExecutionRoleArn:              prev.ExecutionRoleArn,
		InferenceAccelerators:         slices.Clone(prev.InferenceAccelerators), //nolint:staticcheck // deprecated in the SDK, but RegisterTaskDefinition still accepts it and it must be carried over
		IpcMode:                       prev.IpcMode,
		Memory:                        prev.Memory,
		NetworkMode:                   prev.NetworkMode,
		PidMode:                       prev.PidMode,
		PlacementConstraints:          slices.Clone(prev.PlacementConstraints),
		ProxyConfiguration:            prev.ProxyConfiguration,
		RequiresCompatibilities:       slices.Clone(prev.RequiresCompatibilities),
		RuntimePlatform:               prev.RuntimePlatform,
		TaskRoleArn:                   prev.TaskRoleArn,
		TrustedExecutionConfiguration: prev.TrustedExecutionConfiguration,
		Volumes:                       slices.Clone(prev.Volumes),
		Tags:                          mergeTags(prevTags, s),
	}, nil
}

// mergeTags keeps prevTags in order, replaces the stamp keys in place, and appends the
// stamp keys that prevTags did not have, in the order version, run, leg, manifest.
func mergeTags(prevTags []ecstypes.Tag, s Stamp) []ecstypes.Tag {
	stamp := []ecstypes.Tag{
		{Key: aws.String(TagVersion), Value: aws.String(s.Version)},
		{Key: aws.String(TagRun), Value: aws.String(strconv.FormatInt(s.RunID, 10))},
		{Key: aws.String(TagLeg), Value: aws.String(strconv.FormatInt(s.LegID, 10))},
		{Key: aws.String(TagManifest), Value: aws.String(s.ManifestID)},
	}
	stampValue := make(map[string]*string, len(stamp))
	for _, tag := range stamp {
		stampValue[aws.ToString(tag.Key)] = tag.Value
	}

	tags := make([]ecstypes.Tag, 0, len(prevTags)+len(stamp))
	placed := make(map[string]bool, len(stamp))
	for _, tag := range prevTags {
		key := aws.ToString(tag.Key)
		value, isStamp := stampValue[key]
		if !isStamp {
			tags = append(tags, tag)
			continue
		}
		if !placed[key] {
			tags = append(tags, ecstypes.Tag{Key: tag.Key, Value: value})
			placed[key] = true
		}
	}
	for _, tag := range stamp {
		if !placed[aws.ToString(tag.Key)] {
			tags = append(tags, tag)
		}
	}
	return tags
}
