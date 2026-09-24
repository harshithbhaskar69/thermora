//go:build integration

package artifacts

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

func TestFlociReportStorage(t *testing.T) {
	if os.Getenv("THERMORA_FLOCI_TEST") != "1" {
		t.Skip("run task test:cloud against the owned local Floci instance")
	}
	bucket := "thermora-test-" + uuid.NewString()
	store, err := NewLocal("http://127.0.0.1:4566", bucket)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Provision(t.Context()); err != nil {
		t.Fatal(err)
	}
	key := "tenant/device/report.json"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		if _, e := store.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key}); e != nil {
			t.Error(e)
		}
		if _, e := store.client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &bucket}); e != nil {
			t.Error(e)
		}
	})
	if err = store.Provision(t.Context()); err != nil {
		t.Fatal("existing bucket", err)
	}
	data := []byte(`{"provenance":"modelled_comparison","price":-0.08}`)
	for range 2 {
		if err = store.Put(t.Context(), key, data); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Get(t.Context(), key)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("S3 report mismatch", err)
	}
	if err = store.Put(t.Context(), key, make([]byte, MaxBytes+1)); err == nil {
		t.Fatal("oversized report accepted")
	}
	if _, err = store.Get(t.Context(), "missing"); err == nil {
		t.Fatal("missing report returned success")
	}
}
