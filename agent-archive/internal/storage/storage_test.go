package storage

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

func TestVerifyAccessUsesUniqueObjectAndCleansUp(t *testing.T) {
	store := NewMemoryStore()
	if err := VerifyAccess(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	objects, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("setup object was not removed: %#v", objects)
	}
}

func TestPrefixRejectsEscapes(t *testing.T) {
	for _, test := range []struct{ prefix, key string }{
		{"/private", "x"}, {"private/../other", "x"}, {"private", "/x"}, {"private", "../x"}, {"private", `dir\\x`},
	} {
		if _, err := Prefix(test.prefix, test.key); err == nil {
			t.Errorf("Prefix(%q, %q) accepted an unsafe path", test.prefix, test.key)
		}
	}
}

func TestSourceFirstPublicationReusesVerifiedSource(t *testing.T) {
	store := NewMemoryStore()
	source := []byte(`{"schema_version":1}`)
	metadata := []byte(`{"source":"source.abc"}`)
	key := "sessions/codex/id/source." + SHA256Hex(source) + ".json.gz"
	if err := PutSourceThenMetadata(context.Background(), store, key, "sessions/codex/id/metadata.json", source, metadata, RetryPolicy{MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "sessions/codex/id/metadata.json", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := PutSourceThenMetadata(context.Background(), store, key, "sessions/codex/id/metadata.json", source, metadata, RetryPolicy{MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "sessions/codex/id/metadata.json")
	if err != nil || string(got) != string(metadata) {
		t.Fatalf("metadata = %q, err = %v", got, err)
	}
}

func TestS3StoreFakeHTTPRoundTrip(t *testing.T) {
	server := newFakeS3Server()
	defer server.Close()
	awsCfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
	client := NewClient(awsCfg, server.URL, true, 1)
	store, err := NewS3Store(S3StoreOptions{Client: client, Bucket: "archive", Prefix: "agent-archive", MaxGetBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("hello storage")
	if err := store.Put(context.Background(), "sessions/id/source", payload); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "sessions/id/source")
	if err != nil || string(got) != string(payload) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	items, err := store.List(context.Background(), "sessions/id")
	if err != nil || len(items) != 1 || items[0].Key != "sessions/id/source" {
		t.Fatalf("List = %#v, %v", items, err)
	}
	if err := store.Delete(context.Background(), "sessions/id/source"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), "sessions/id/source"); err != ErrNotFound {
		t.Fatalf("missing Get error = %v", err)
	}
}

type flakyStore struct {
	*MemoryStore
	mu       sync.Mutex
	failPuts int
}

func (s *flakyStore) Put(ctx context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPuts > 0 {
		s.failPuts--
		return io.ErrUnexpectedEOF
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func TestSourcePublicationRetriesAndPublishesMetadataLast(t *testing.T) {
	store := &flakyStore{MemoryStore: NewMemoryStore(), failPuts: 2}
	err := PutSourceThenMetadata(context.Background(), store, "source.hash", "metadata.json", []byte("source"), []byte("metadata"), RetryPolicy{MaxAttempts: 3, InitialWait: time.Nanosecond, MaxWait: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := store.List(context.Background(), "")
	if err != nil || len(objects) != 2 {
		t.Fatalf("objects = %#v, err = %v", objects, err)
	}
}

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeS3Server() *httptest.Server {
	fake := &fakeS3{objects: make(map[string][]byte)}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/archive/")
		fake.mu.Lock()
		defer fake.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			fake.objects[key] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if r.URL.Query().Has("list-type") {
				writeListResponse(w, fake.objects, r.URL.Query().Get("prefix"))
				return
			}
			body, ok := fake.objects[key]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case http.MethodDelete:
			delete(fake.objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
}

func writeListResponse(w http.ResponseWriter, objects map[string][]byte, prefix string) {
	type content struct {
		Key  string `xml:"Key"`
		Size int    `xml:"Size"`
	}
	type result struct {
		XMLName  xml.Name  `xml:"ListBucketResult"`
		Contents []content `xml:"Contents"`
	}
	var out result
	for key, value := range objects {
		if strings.HasPrefix(key, prefix) {
			out.Contents = append(out.Contents, content{Key: key, Size: len(value)})
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(out)
}
