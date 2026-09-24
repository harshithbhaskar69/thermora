// Package artifacts stores immutable local reports through the AWS S3 SDK.
package artifacts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

const (
	Bucket   = "thermora-reports"
	MaxBytes = 1024 * 1024
)

type Store struct {
	client *s3.Client
	bucket string
}

// NewLocal has no ambient AWS credential or endpoint chain and never permits a real AWS endpoint.
func NewLocal(endpoint, bucket string) (*Store, error) {
	if endpoint != "http://127.0.0.1:4566" && endpoint != "http://floci:4566" {
		return nil, errors.New("report storage requires an explicit local Floci endpoint")
	}
	if !regexp.MustCompile(`^thermora-[a-z0-9-]{1,45}$`).MatchString(bucket) {
		return nil, errors.New("invalid local report bucket")
	}
	client := s3.New(s3.Options{
		Region:           "us-east-1",
		BaseEndpoint:     &endpoint,
		UsePathStyle:     true,
		RetryMaxAttempts: 2,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test", Source: "thermora-local"}, nil
		}),
		HTTPClient: &http.Client{
			Timeout:       3 * time.Second,
			Transport:     &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	})
	return &Store{client: client, bucket: bucket}, nil
}

func (s *Store) Provision(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket})
	if err == nil {
		return nil
	}
	var apiError smithy.APIError
	if !errors.As(err, &apiError) || (apiError.ErrorCode() != "NotFound" && apiError.ErrorCode() != "NoSuchBucket") {
		return fmt.Errorf("inspect local report bucket: %w", err)
	}
	_, err = s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &s.bucket})
	return err
}

func (s *Store) Put(ctx context.Context, key string, data []byte) error {
	if len(data) > MaxBytes {
		return errors.New("report exceeds 1 MiB")
	}
	_, err := s.client.PutObject(
		ctx,
		&s3.PutObjectInput{
			Bucket:      &s.bucket,
			Key:         &key,
			Body:        bytes.NewReader(data),
			ContentType: new("application/json"),
		},
	)
	return err
}

func (s *Store) Get(ctx context.Context, key string) (data []byte, err error) {
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, result.Body.Close()) }()
	data, err = io.ReadAll(io.LimitReader(result.Body, MaxBytes+1))
	if len(data) > MaxBytes {
		return nil, errors.New("stored report exceeds 1 MiB")
	}
	return data, err
}
