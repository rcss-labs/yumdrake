package artifacts_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rcss-labs/yamdrake/internal/artifacts"
)

type recorder struct{ header http.Header }

func (r *recorder) Do(req *http.Request) (*http.Response, error) {
	r.header = req.Header.Clone()
	_, _ = io.Copy(io.Discard, req.Body)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func TestPutRequestsServerSideEncryption(t *testing.T) {
	for name, tc := range map[string]struct{ kmsKey, wantAlgo, wantKey string }{
		"s3 managed": {"", "AES256", ""},
		"kms":        {"key-123", "aws:kms", "key-123"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			client := s3.New(s3.Options{
				Region: "us-east-1", HTTPClient: rec, UsePathStyle: true,
				Credentials:  credentials.NewStaticCredentialsProvider("a", "b", ""),
				BaseEndpoint: aws.String("http://example.invalid"),
			})
			if _, _, err := artifacts.New(client, "bkt", tc.kmsKey).Put(context.Background(), 1, "svc", "values", "a.yaml", false, []byte("x")); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if got := rec.header.Get("X-Amz-Server-Side-Encryption"); got != tc.wantAlgo {
				t.Errorf("SSE header = %q, want %q", got, tc.wantAlgo)
			}
			if got := rec.header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"); got != tc.wantKey {
				t.Errorf("KMS key header = %q, want %q", got, tc.wantKey)
			}
		})
	}
}

// bodyClient answers every request with a fixed body and optional content length.
type bodyClient struct {
	body          func() io.Reader
	contentLength int64
}

func (b *bodyClient) Do(req *http.Request) (*http.Response, error) {
	h := http.Header{}
	if b.contentLength >= 0 {
		h.Set("Content-Length", strconv.FormatInt(b.contentLength, 10))
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(b.body()), Header: h, ContentLength: b.contentLength}, nil
}

func stubStore(hc aws.HTTPClient) *artifacts.S3Store {
	client := s3.New(s3.Options{
		Region: "us-east-1", HTTPClient: hc, UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("a", "b", ""),
		BaseEndpoint: aws.String("http://example.invalid"),
	})
	return artifacts.New(client, "bkt", "")
}

func TestGetRefusesObjectsOverTheLimit(t *testing.T) {
	// A body with no declared length that never ends: Get must stop reading.
	endless := &bodyClient{body: func() io.Reader { return zeros{} }, contentLength: -1}
	_, err := stubStore(endless).Get(context.Background(), "manifests/1/s/k/n", "00")
	if !errors.Is(err, artifacts.ErrTooLarge) || errors.Is(err, artifacts.ErrCorrupt) {
		t.Fatalf("endless body: %v, want ErrTooLarge only", err)
	}
	// A declared length over the cap is refused too.
	declared := &bodyClient{body: func() io.Reader { return strings.NewReader("x") }, contentLength: artifacts.MaxObjectBytes + 1}
	_, err = stubStore(declared).Get(context.Background(), "manifests/1/s/k/n", "00")
	if !errors.Is(err, artifacts.ErrTooLarge) {
		t.Fatalf("declared length: %v, want ErrTooLarge", err)
	}
	if strings.Contains(err.Error(), "manifests/1/s/k/n") == false {
		t.Errorf("error should name the key: %v", err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { return len(p), nil }

func TestPutRefusesDataOverTheLimit(t *testing.T) {
	rec := &recorder{}
	_, _, err := stubStore(rec).Put(context.Background(), 1, "s", "k", "n", false, make([]byte, artifacts.MaxObjectBytes+1))
	if !errors.Is(err, artifacts.ErrTooLarge) {
		t.Fatalf("Put over the cap: %v, want ErrTooLarge", err)
	}
	if rec.header != nil {
		t.Error("Put sent a request for oversized data")
	}
}

func TestPutRejectsDotAndControlCharacters(t *testing.T) {
	for _, bad := range []string{".", "a\x00b", "a\nb", "a\x7fb", "a\tb"} {
		if _, _, err := stubStore(&recorder{}).Put(context.Background(), 1, bad, "k", "n", false, []byte("x")); err == nil {
			t.Errorf("service %q accepted", bad)
		}
	}
}
