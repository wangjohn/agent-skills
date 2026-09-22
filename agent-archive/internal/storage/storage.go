// Package storage provides the small object-store contract used by the archive.
//
// Object keys passed to this package are relative to the configured bucket
// prefix. Implementations do not interpret JSON or archive metadata; keeping
// that boundary here makes it possible to use the collector with a fake or
// in-memory store in tests.
package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrNotFound is returned by Get when an object does not exist.
	ErrNotFound = errors.New("object not found")
	// ErrChecksumMismatch means the bytes read from storage do not match the
	// checksum supplied by the caller.
	ErrChecksumMismatch = errors.New("object checksum mismatch")
	// ErrObjectTooLarge is returned when a remote object exceeds the bounded
	// read limit configured on S3Store.
	ErrObjectTooLarge = errors.New("object exceeds read limit")
)

// ObjectStore is the storage boundary used by the archive collector and
// reader. Put replaces an object at key atomically from the caller's point of
// view. Implementations should not mutate data after Put returns.
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	List(ctx context.Context, prefix string) ([]Object, error)
	Delete(ctx context.Context, key string) error
}

// ObjectKeyer is implemented by stores that compose a full object key from a
// relative archive key, such as S3Store applying its configured bucket
// prefix. VerifyAccess uses it to report the exact object key in errors so a
// user can find and remove a leftover setup-test object by hand.
type ObjectKeyer interface {
	ObjectKey(relative string) string
}

// Object is a listed object and intentionally contains no body. Downloading
// content is an explicit Get operation.
type Object struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

// Prefix joins a configured bucket prefix with a relative archive key.
// Prefix returns a slash-normalized key and rejects absolute keys so callers
// cannot accidentally escape their configured namespace.
func Prefix(prefix, key string) (string, error) {
	if strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "\\") || hasDotComponent(prefix) {
		return "", fmt.Errorf("invalid object prefix %q", prefix)
	}
	if strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || key == "" || hasDotComponent(key) {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return key, nil
	}
	return prefix + "/" + key, nil
}

// ReadAndVerify downloads key and verifies its SHA-256 hex digest. The
// checksum covers the exact bytes returned by Get, rather than an ETag (which
// is not a SHA-256 checksum for multipart or encrypted objects).
func ReadAndVerify(ctx context.Context, store ObjectStore, key, sha256Hex string) ([]byte, error) {
	b, err := store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if !VerifySHA256(b, sha256Hex) {
		return nil, fmt.Errorf("%w for %q", ErrChecksumMismatch, key)
	}
	return b, nil
}

// VerifySHA256 reports whether data's SHA-256 digest equals expectedHex.
func VerifySHA256(data []byte, expectedHex string) bool {
	got := SHA256Hex(data)
	return strings.EqualFold(got, strings.TrimSpace(expectedHex))
}

// SHA256Hex returns the lower-case SHA-256 digest of data.
func SHA256Hex(data []byte) string {
	return sha256Hex(data)
}

// VerifyAccess performs the setup round trip required by the product spec.
// It creates a unique relative object key and the store applies its
// configured prefix. It reads and verifies the object, confirms it appears in
// List, and removes it. No bucket-admin operation is required. Errors name
// the full object key when the store implements ObjectKeyer.
func VerifyAccess(ctx context.Context, store ObjectStore) error {
	key := uniqueSetupKey()
	label := setupKeyLabel(store, key)
	payload := []byte(`{"agent_archive_setup_test":true}`)
	cleanup := func() error { return store.Delete(ctx, key) }
	if err := store.Put(ctx, key, payload); err != nil {
		return withCleanupError(fmt.Errorf("setup test upload %s: %w", label, err), cleanup, label)
	}
	got, err := store.Get(ctx, key)
	if err != nil {
		return withCleanupError(fmt.Errorf("setup test read %s: %w", label, err), cleanup, label)
	}
	if string(got) != string(payload) {
		return withCleanupError(fmt.Errorf("setup test read %s: %w", label, ErrChecksumMismatch), cleanup, label)
	}
	objects, err := store.List(ctx, key)
	if err != nil {
		return withCleanupError(fmt.Errorf("setup test list %s: %w", label, err), cleanup, label)
	}
	found := false
	for _, obj := range objects {
		if obj.Key == key {
			found = true
			break
		}
	}
	if !found {
		return withCleanupError(fmt.Errorf("setup test list %s: object missing", label), cleanup, label)
	}
	if err := cleanup(); err != nil {
		return fmt.Errorf("setup test cleanup %s: %w", label, err)
	}
	return nil
}

// setupKeyLabel returns the quoted object key for error messages: the full
// key as the store composes it when available, otherwise the relative key
// with a note that the configured prefix is not shown.
func setupKeyLabel(store ObjectStore, key string) string {
	if keyer, ok := store.(ObjectKeyer); ok {
		return fmt.Sprintf("%q", keyer.ObjectKey(key))
	}
	return fmt.Sprintf("%q (relative to the configured prefix)", key)
}

func withCleanupError(primary error, cleanup func() error, label string) error {
	if cleanupErr := cleanup(); cleanupErr != nil {
		return fmt.Errorf("%w; setup test cleanup %s also failed: %v", primary, label, cleanupErr)
	}
	return primary
}

func hasDotComponent(value string) bool {
	for _, component := range strings.Split(value, "/") {
		if component == "." || component == ".." {
			return true
		}
	}
	return false
}

// PutSourceThenMetadata implements source-first publication. The source is
// uploaded and read back with checksum verification before metadata is
// published. Retry attempts reuse the exact input bytes, so a retry cannot
// produce another source hash or timestamp.
func PutSourceThenMetadata(ctx context.Context, store ObjectStore, sourceKey, metadataKey string, source, metadata []byte, retry RetryPolicy) error {
	if sourceKey == "" || metadataKey == "" {
		return errors.New("source and metadata keys are required")
	}
	if sourceKey == metadataKey {
		return errors.New("source and metadata keys must differ")
	}
	if err := retry.run(ctx, func() error {
		if existing, getErr := store.Get(ctx, sourceKey); getErr == nil {
			if VerifySHA256(existing, SHA256Hex(source)) {
				return nil
			}
			return fmt.Errorf("%w for existing source %q", ErrChecksumMismatch, sourceKey)
		} else if !errors.Is(getErr, ErrNotFound) {
			return getErr
		}
		if err := store.Put(ctx, sourceKey, source); err != nil {
			return err
		}
		_, err := ReadAndVerify(ctx, store, sourceKey, SHA256Hex(source))
		return err
	}); err != nil {
		return fmt.Errorf("publish source %q: %w", sourceKey, err)
	}
	if err := retry.run(ctx, func() error { return store.Put(ctx, metadataKey, metadata) }); err != nil {
		return fmt.Errorf("publish metadata %q: %w", metadataKey, err)
	}
	return nil
}

// RetryPolicy controls bounded retries for transient storage operations.
// MaxAttempts includes the first attempt. Zero uses the default of three.
type RetryPolicy struct {
	MaxAttempts int
	InitialWait time.Duration
	MaxWait     time.Duration
}

func (p RetryPolicy) run(ctx context.Context, operation func() error) error {
	attempts := p.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	wait := p.InitialWait
	if wait <= 0 {
		wait = 25 * time.Millisecond
	}
	maxWait := p.MaxWait
	if maxWait <= 0 {
		maxWait = time.Second
	}
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if err = operation(); err == nil {
			return nil
		}
		if attempt+1 == attempts {
			break
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		wait *= 2
		if wait > maxWait {
			wait = maxWait
		}
	}
	return err
}
