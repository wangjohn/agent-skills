// Package credentials resolves archive storage credentials without copying
// secrets into archive configuration or process arguments.
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

var (
	ErrUnavailable       = errors.New("credential store unavailable")
	ErrInvalidReference  = errors.New("invalid credential reference")
	ErrInvalidProfile    = errors.New("invalid AWS profile")
	ErrMissingCredential = errors.New("credential is missing")
)

// KeychainService is the canonical macOS Keychain service name this archive
// uses for every R2CredentialRef, so a hook, the collector, and setup all
// resolve the same stored item.
const KeychainService = "agent-archive"

// R2Credentials are intentionally only accepted through a Keychain-backed
// reference in production setup. They are value types so callers can inject a
// test credential provider without any shell or command-line transport.
type R2Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

func (c R2Credentials) validate() error {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return ErrMissingCredential
	}
	return nil
}

// CredentialStore stores and resolves opaque references. Implementations must
// never include secret values in errors or diagnostic output.
type CredentialStore interface {
	Save(ctx context.Context, reference string, value R2Credentials) error
	Load(ctx context.Context, reference string) (R2Credentials, error)
	Delete(ctx context.Context, reference string) error
}

// Config describes one archive storage destination. For S3, AWSProfile is
// mandatory and is loaded deterministically. For R2, R2CredentialRef points
// to a Keychain item and Endpoint may be omitted when AccountID is supplied.
type Config struct {
	Provider        string
	Bucket          string
	Region          string
	Prefix          string
	AWSProfile      string
	R2CredentialRef string
	R2AccountID     string
	R2Endpoint      string
}

const (
	ProviderS3 = "s3"
	ProviderR2 = "r2"
)

// LoadAWSConfig loads exactly the selected shared AWS profile. Supplying an
// explicit profile makes the SDK resolve that profile's static, SSO,
// process, or role credentials; it does not fall back to unrelated environment
// credentials when the selected profile is unavailable.
func LoadAWSConfig(ctx context.Context, profile, region string) (aws.Config, error) {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return aws.Config{}, ErrInvalidProfile
	}
	options := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithSharedConfigProfile(profile),
	}
	if strings.TrimSpace(region) != "" {
		options = append(options, awsconfig.WithRegion(strings.TrimSpace(region)))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS profile %q: %w", profile, err)
	}
	return cfg, nil
}

// LoadR2Config creates an AWS config using a Keychain-resolved static
// provider. Region is always "auto", as required by Cloudflare R2. Endpoint
// is normalized and may be derived from a Cloudflare account ID.
func LoadR2Config(ctx context.Context, cfg Config, store CredentialStore) (aws.Config, string, error) {
	if store == nil {
		return aws.Config{}, "", ErrUnavailable
	}
	if cfg.R2CredentialRef == "" {
		return aws.Config{}, "", ErrInvalidReference
	}
	value, err := store.Load(ctx, cfg.R2CredentialRef)
	if err != nil {
		return aws.Config{}, "", err
	}
	if err := value.validate(); err != nil {
		return aws.Config{}, "", err
	}
	endpoint, err := R2Endpoint(cfg.R2Endpoint, cfg.R2AccountID)
	if err != nil {
		return aws.Config{}, "", err
	}
	provider := awscredentials.NewStaticCredentialsProvider(value.AccessKeyID, value.SecretAccessKey, value.SessionToken)
	return aws.Config{Region: "auto", Credentials: provider}, endpoint, nil
}

// R2Endpoint returns a validated endpoint. Cloudflare's account endpoint is
// inferred only when the caller explicitly supplies an account ID.
func R2Endpoint(endpoint, accountID string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" || strings.ContainsAny(accountID, "/\\ \t\r\n") {
			return "", errors.New("R2 endpoint or account ID is required")
		}
		endpoint = "https://" + accountID + ".r2.cloudflarestorage.com"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid R2 endpoint")
	}
	return strings.TrimRight(endpoint, "/"), nil
}

// EncodeSecret is used by KeychainStore and is exported solely so a test can
// verify that the stored representation contains no JSON configuration.
func EncodeSecret(value R2Credentials) ([]byte, error) {
	if err := value.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func DecodeSecret(data []byte) (R2Credentials, error) {
	var value R2Credentials
	if err := json.Unmarshal(data, &value); err != nil {
		return R2Credentials{}, ErrUnavailable
	}
	if err := value.validate(); err != nil {
		return R2Credentials{}, err
	}
	return value, nil
}
