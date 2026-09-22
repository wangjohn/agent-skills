package storage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

func TestVerifyAccessConfiguredS3Namespace(t *testing.T) {
	for _, prefix := range []string{"", "agent-archive", "nested/archive/"} {
		t.Run(prefix, func(t *testing.T) {
			backend := newFakeS3Server()
			defer backend.Close()
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				key := strings.TrimPrefix(r.URL.Path, "/archive/")
				if r.URL.Query().Has("list-type") {
					key = r.URL.Query().Get("prefix")
				}
				paths = append(paths, key)
				backend.Config.Handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test", "test", "")}
			store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, server.URL, true, 1), Bucket: "archive", Prefix: prefix})
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyAccess(context.Background(), store); err != nil {
				t.Fatal(err)
			}
			expected := ".setup-test/"
			if prefix != "" {
				expected = strings.TrimSuffix(prefix, "/") + "/" + expected
			}
			if len(paths) != 4 {
				t.Fatalf("requests = %v", paths)
			}
			for _, path := range paths {
				if !strings.HasPrefix(path, expected) || path != paths[0] {
					t.Fatalf("unexpected probe namespace: %v, want %s", paths, expected)
				}
			}
			items, err := store.List(context.Background(), "")
			if err != nil || len(items) != 0 {
				t.Fatalf("probe not cleaned up: %v %v", items, err)
			}
		})
	}
}

func TestVerifyAccessCleanupFailureReportsFullObjectKey(t *testing.T) {
	backend := newFakeS3Server()
	defer backend.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
			return
		}
		backend.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test", "test", "")}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, server.URL, true, 1), Bucket: "archive", Prefix: "nested/archive/"})
	if err != nil {
		t.Fatal(err)
	}
	err = VerifyAccess(context.Background(), store)
	if err == nil {
		t.Fatal("expected cleanup failure")
	}
	if !strings.Contains(err.Error(), `setup test cleanup "nested/archive/.setup-test/`) {
		t.Fatalf("cleanup error should name the prefixed object key: %v", err)
	}
	if strings.Contains(err.Error(), "relative to the configured prefix") {
		t.Fatalf("S3 store should report the full key without a note: %v", err)
	}
}

func TestVerifyAccessWithoutObjectKeyerNotesRelativeKey(t *testing.T) {
	store := failingDeleteStore{NewMemoryStore()}
	err := VerifyAccess(context.Background(), store)
	if err == nil {
		t.Fatal("expected cleanup failure")
	}
	if !strings.Contains(err.Error(), `.setup-test/`) || !strings.Contains(err.Error(), "relative to the configured prefix") {
		t.Fatalf("cleanup error should name the relative key and note the prefix: %v", err)
	}
}

type failingDeleteStore struct{ *MemoryStore }

func (failingDeleteStore) Delete(context.Context, string) error {
	return errors.New("delete denied")
}
