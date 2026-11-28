//go:build integration && floci

package artifacts_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/test/awstest"
)

const region = "us-east-1"

// newStore seeds a fresh account and bucket and returns a store over them.
func newStore(t *testing.T, kmsKeyID string) (*artifacts.S3Store, *awstest.Account, string, *s3.Client) {
	t.Helper()
	w := awstest.New(t)
	acct := w.Account(awstest.RandomAccountID())
	bucket := "yd-artifacts-" + awstest.RandomSuffix()
	acct.Bucket(bucket, false)
	client := s3.NewFromConfig(acct.AWSConfig(region), func(o *s3.Options) { o.UsePathStyle = true })
	return artifacts.New(client, bucket, kmsKeyID), acct, bucket, client
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestPutGetRoundTripAndChecksum(t *testing.T) {
	ctx := context.Background()
	store, acct, bucket, client := newStore(t, "")
	data := []byte("replicas: 3\n")

	key, digest, err := store.Put(ctx, 42, "web", "values", "app.yaml", false, data)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := "manifests/42/web/values/app.yaml"; key != want {
		t.Errorf("key = %q, want %q", key, want)
	}
	if digest != sum(data) {
		t.Errorf("sha256 = %q, want %q", digest, sum(data))
	}
	if got, ok := acct.Object(bucket, key); !ok || !bytes.Equal(got, data) {
		t.Errorf("stored object = %q, %v", got, ok)
	}
	got, err := store.Get(ctx, key, digest)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if _, err := store.Get(ctx, key, sum([]byte("other"))); !errors.Is(err, artifacts.ErrCorrupt) {
		t.Errorf("Get with a wrong checksum: %v, want ErrCorrupt", err)
	}

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if head.ServerSideEncryption != s3types.ServerSideEncryptionAes256 {
		t.Errorf("ServerSideEncryption = %q, want AES256", head.ServerSideEncryption)
	}
}

func TestPutEncryptsWithKMSKeyWhenGiven(t *testing.T) {
	ctx := context.Background()
	w := awstest.New(t)
	acct := w.Account(awstest.RandomAccountID())
	bucket := "yd-artifacts-" + awstest.RandomSuffix()
	acct.Bucket(bucket, false)
	keyID := acct.KMSKey(region)
	client := s3.NewFromConfig(acct.AWSConfig(region), func(o *s3.Options) { o.UsePathStyle = true })

	key, digest, err := artifacts.New(client, bucket, keyID).Put(ctx, 1, "web", "values", "a.yaml", false, []byte("a: 1\n"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if head.ServerSideEncryption != s3types.ServerSideEncryptionAwsKms || !strings.Contains(aws.ToString(head.SSEKMSKeyId), keyID) {
		t.Errorf("encryption = %q key %q, want aws:kms with %q", head.ServerSideEncryption, aws.ToString(head.SSEKMSKeyId), keyID)
	}
	if _, err := artifacts.New(client, bucket, keyID).Get(ctx, key, digest); err != nil {
		t.Errorf("Get: %v", err)
	}
}

func TestGetDetectsCorruption(t *testing.T) {
	ctx := context.Background()
	store, acct, bucket, _ := newStore(t, "")
	secret := "super-secret-contents"
	key, digest, err := store.Put(ctx, 7, "api", "values", "a.yaml", false, []byte("good: true\n"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	acct.PutObject(bucket, key, []byte(secret))

	got, err := store.Get(ctx, key, digest)
	if !errors.Is(err, artifacts.ErrCorrupt) {
		t.Fatalf("Get after tampering: %v, want ErrCorrupt", err)
	}
	if got != nil {
		t.Errorf("Get returned %d bytes with the error", len(got))
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks object contents: %v", err)
	}
}

func TestPreviousCopiesUseSeparateKeys(t *testing.T) {
	ctx := context.Background()
	store, _, _, _ := newStore(t, "")
	current, previous := []byte("new: 1\n"), []byte("old: 1\n")

	curKey, curSum, err := store.Put(ctx, 9, "web", "values", "app.yaml", false, current)
	if err != nil {
		t.Fatalf("Put current: %v", err)
	}
	prevKey, prevSum, err := store.Put(ctx, 9, "web", "values", "app.yaml", true, previous)
	if err != nil {
		t.Fatalf("Put previous: %v", err)
	}
	if want := "manifests/9/web/values/previous/app.yaml"; prevKey != want {
		t.Errorf("previous key = %q, want %q", prevKey, want)
	}
	if curKey == prevKey {
		t.Fatal("current and previous share a key")
	}
	if got, err := store.Get(ctx, curKey, curSum); err != nil || !bytes.Equal(got, current) {
		t.Errorf("current = %q, %v", got, err)
	}
	if got, err := store.Get(ctx, prevKey, prevSum); err != nil || !bytes.Equal(got, previous) {
		t.Errorf("previous = %q, %v", got, err)
	}

	for _, bad := range [][4]string{{"", "values", "a", ""}, {"web", "val/ues", "a", ""}, {"web", "values", "..", ""}, {"web", "values", "a/b", ""}, {"..", "values", "a", ""}} {
		if _, _, err := store.Put(ctx, 9, bad[0], bad[1], bad[2], false, current); err == nil {
			t.Errorf("Put accepted components %q", bad)
		}
	}
	if _, err := store.Get(ctx, "other/9/web/values/a", curSum); err == nil {
		t.Error("Get accepted a key outside manifests/")
	}
}
