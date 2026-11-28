package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Store keeps config copies in one encrypted S3 bucket.
type S3Store struct {
	client   *s3.Client
	bucket   string
	kmsKeyID string
}

var _ Store = (*S3Store)(nil)

// New returns an S3Store. With a non-empty kmsKeyID objects are encrypted with
// that KMS key; otherwise with S3-managed keys (SSE-S3).
func New(client *s3.Client, bucket, kmsKeyID string) *S3Store {
	return &S3Store{client: client, bucket: bucket, kmsKeyID: kmsKeyID}
}

// Put stores data under manifests/<run>/<service>/<kind>/<name>, or under
// .../<kind>/previous/<name> when previous is true (the prior destination
// content). It returns the key and the hex SHA-256 of data.
func (s *S3Store) Put(ctx context.Context, runID int64, service, kind, name string, previous bool, data []byte) (key, sha256hex string, err error) {
	if len(data) > MaxObjectBytes {
		return "", "", fmt.Errorf("%w: %d bytes, limit %d", ErrTooLarge, len(data), MaxObjectBytes)
	}
	key, err = objectKey(runID, service, kind, name, previous)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(data)
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	}
	if s.kmsKeyID != "" {
		input.ServerSideEncryption = s3types.ServerSideEncryptionAwsKms
		input.SSEKMSKeyId = aws.String(s.kmsKeyID)
	} else {
		input.ServerSideEncryption = s3types.ServerSideEncryptionAes256
	}
	if _, err := s.client.PutObject(ctx, input); err != nil {
		return "", "", fmt.Errorf("artifacts: put %s: %w", key, err)
	}
	return key, hex.EncodeToString(sum[:]), nil
}

// Get returns the object's bytes after checking they hash to wantSHA256
// (hex). It returns ErrCorrupt, and no bytes, when they do not.
func (s *S3Store) Get(ctx context.Context, key, wantSHA256 string) ([]byte, error) {
	if !strings.HasPrefix(key, keyPrefix) || strings.Contains(key, "..") {
		return nil, fmt.Errorf("artifacts: key %q is outside %s", key, keyPrefix)
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("artifacts: get %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	if aws.ToInt64(out.ContentLength) > MaxObjectBytes {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, key, MaxObjectBytes)
	}
	data, err := io.ReadAll(io.LimitReader(out.Body, MaxObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("artifacts: read %s: %w", key, err)
	}
	if len(data) > MaxObjectBytes {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, key, MaxObjectBytes)
	}
	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), wantSHA256) {
		return nil, fmt.Errorf("%w: %s", ErrCorrupt, key)
	}
	return data, nil
}
