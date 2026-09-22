package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsretry "github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
)

// S3Store is an ObjectStore backed by Amazon S3 or a compatible endpoint such
// as Cloudflare R2. The SDK client is injected so tests can use a fake HTTP
// server without credentials or a bucket administrator account.
type S3Store struct {
	client      *s3.Client
	bucket      string
	prefix      string
	maxGetBytes int64
}

// S3StoreOptions configures a store. Client must be constructed with the
// desired credential provider; this package never reads credentials itself.
type S3StoreOptions struct {
	Client       *s3.Client
	Bucket       string
	Prefix       string
	MaxAttempts  int
	Endpoint     string
	UsePathStyle bool
	// MaxGetBytes bounds memory used while reading an object. Zero selects
	// the 64 MiB default appropriate for archive metadata and source bundles.
	MaxGetBytes int64
}

// NewS3Store creates a store from an AWS SDK client. Endpoint and
// UsePathStyle are accepted here as convenience for callers constructing a
// client through NewClient; an already-created client is always used as-is.
func NewS3Store(options S3StoreOptions) (*S3Store, error) {
	if options.Client == nil {
		return nil, errors.New("storage: S3 client is required")
	}
	if strings.TrimSpace(options.Bucket) == "" {
		return nil, errors.New("storage: bucket is required")
	}
	if options.Prefix != "" {
		if _, err := Prefix(options.Prefix, "archive"); err != nil {
			return nil, err
		}
	}
	maxGetBytes := options.MaxGetBytes
	if maxGetBytes <= 0 {
		maxGetBytes = 64 << 20
	}
	return &S3Store{client: options.Client, bucket: options.Bucket, prefix: strings.Trim(options.Prefix, "/"), maxGetBytes: maxGetBytes}, nil
}

// NewClient constructs an S3 client for AWS or an S3-compatible endpoint.
// For R2 callers should pass region "auto", an endpoint supplied by
// Cloudflare, and a static credentials provider from internal/credentials.
// Path style addressing is used for compatibility with both R2 and local
// fake servers.
func NewClient(cfg aws.Config, endpoint string, pathStyle bool, maxAttempts int) *s3.Client {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return s3.NewFromConfig(cfg, func(options *s3.Options) {
		if endpoint != "" {
			options.BaseEndpoint = aws.String(strings.TrimRight(endpoint, "/"))
		}
		options.UsePathStyle = pathStyle
		options.Retryer = awsretry.NewStandard(func(retryOptions *awsretry.StandardOptions) {
			retryOptions.MaxAttempts = maxAttempts
		})
	})
}

func (s *S3Store) key(relative string) (string, error) {
	return Prefix(s.prefix, relative)
}

// ObjectKey returns the full bucket key for relative, including the
// configured prefix. Keys that Prefix rejects are returned unchanged so error
// messages still identify the object.
func (s *S3Store) ObjectKey(relative string) string {
	key, err := s.key(relative)
	if err != nil {
		return relative
	}
	return key
}

func (s *S3Store) Put(ctx context.Context, relative string, data []byte) error {
	key, err := s.key(relative)
	if err != nil {
		return err
	}
	sum := sha256Bytes(data)
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:         aws.String(s.bucket),
		Key:            aws.String(key),
		Body:           bytes.NewReader(data),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	})
	return err
}

func (s *S3Store) Get(ctx context.Context, relative string) ([]byte, error) {
	key, err := s.key(relative)
	if err != nil {
		return nil, err
	}
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer output.Body.Close()
	limited := io.LimitReader(output.Body, s.maxGetBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.maxGetBytes {
		return nil, fmt.Errorf("%w: %q exceeds %d bytes", ErrObjectTooLarge, relative, s.maxGetBytes)
	}
	return data, nil
}

func (s *S3Store) List(ctx context.Context, relativePrefix string) ([]Object, error) {
	prefix, err := s.keyForList(relativePrefix)
	if err != nil {
		return nil, err
	}
	pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix)})
	var objects []Object
	for pager.HasMorePages() {
		page, pageErr := pager.NextPage(ctx)
		if pageErr != nil {
			return nil, pageErr
		}
		for _, item := range page.Contents {
			if item.Key == nil {
				continue
			}
			obj := Object{Key: trimStorePrefix(*item.Key, s.prefix)}
			if item.Size != nil {
				obj.Size = *item.Size
			}
			if item.ETag != nil {
				obj.ETag = strings.Trim(*item.ETag, "\"")
			}
			if item.LastModified != nil {
				obj.LastModified = *item.LastModified
			}
			objects = append(objects, obj)
		}
	}
	return objects, nil
}

func (s *S3Store) keyForList(relativePrefix string) (string, error) {
	if strings.TrimSpace(relativePrefix) == "" {
		if s.prefix == "" {
			return "", nil
		}
		return s.prefix + "/", nil
	}
	return s.key(relativePrefix)
}

func (s *S3Store) Delete(ctx context.Context, relative string) error {
	key, err := s.key(relative)
	if err != nil {
		return err
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

func trimStorePrefix(key, prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return key
	}
	return strings.TrimPrefix(strings.TrimPrefix(key, prefix), "/")
}

func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "nosuchkey") || strings.Contains(message, "not found") || strings.Contains(message, "status code: 404")
}

func sha256Bytes(data []byte) [32]byte {
	// Kept separate from the hex helper so Put can pass the standard S3
	// base64 checksum header without a second encoding round trip.
	return sha256Sum(data)
}

var _ ObjectStore = (*S3Store)(nil)

// NewConfiguredStore resolves the selected profile or Keychain reference and
// builds the common S3 client used for both providers. It is deliberately
// small so CLI setup can perform its synthetic round trip without knowing SDK
// credential details.
func NewConfiguredStore(ctx context.Context, cfg credentials.Config, keychain credentials.CredentialStore) (*S3Store, error) {
	var awsCfg aws.Config
	var endpoint string
	var err error
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "s3":
		region := cfg.Region
		if strings.TrimSpace(region) == "" {
			return nil, errors.New("storage: AWS region is required")
		}
		awsCfg, err = credentials.LoadAWSConfig(ctx, cfg.AWSProfile, region)
	case "r2":
		awsCfg, endpoint, err = credentials.LoadR2Config(ctx, cfg, keychain)
	default:
		return nil, fmt.Errorf("storage: unsupported provider %q", cfg.Provider)
	}
	if err != nil {
		return nil, err
	}
	client := NewClient(awsCfg, endpoint, true, 3)
	return NewS3Store(S3StoreOptions{Client: client, Bucket: cfg.Bucket, Prefix: cfg.Prefix, Endpoint: endpoint, UsePathStyle: true})
}
