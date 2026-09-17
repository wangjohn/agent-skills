package credentials

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestR2EndpointCanBeDerived(t *testing.T) {
	endpoint, err := R2Endpoint("", "account-123")
	if err != nil || endpoint != "https://account-123.r2.cloudflarestorage.com" {
		t.Fatalf("endpoint = %q, err = %v", endpoint, err)
	}
	for _, endpoint := range []string{
		"https://example.test/path",
		"http://example.test",
		"https://example.test?token=secret",
		"https://example.test/#fragment",
	} {
		if _, err := R2Endpoint(endpoint, ""); err == nil {
			t.Fatalf("accepted invalid endpoint %q", endpoint)
		}
	}
}

func TestLoadAWSConfigHonorsExplicitProfileOverEnvironmentCredentials(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config")
	credentialsFile := filepath.Join(dir, "credentials")
	if err := os.WriteFile(configFile, []byte("[profile archive]\nregion = eu-west-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsFile, []byte("[archive]\naws_access_key_id = profile-key\naws_secret_access_key = profile-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsFile)
	t.Setenv("AWS_ACCESS_KEY_ID", "unrelated-env-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "unrelated-env-secret")
	cfg, err := LoadAWSConfig(context.Background(), "archive", "")
	if err != nil {
		t.Fatal(err)
	}
	value, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if value.AccessKeyID != "profile-key" || value.SecretAccessKey != "profile-secret" {
		t.Fatalf("resolved unrelated credentials: %#v", value)
	}
}

func TestLoadAWSConfigDoesNotFallBackWhenSelectedProfileMissing(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config")
	credentialsFile := filepath.Join(dir, "credentials")
	if err := os.WriteFile(configFile, []byte("[profile other]\nregion = us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsFile, []byte("[other]\naws_access_key_id = other-key\naws_secret_access_key = other-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configFile)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credentialsFile)
	t.Setenv("AWS_ACCESS_KEY_ID", "unrelated-env-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "unrelated-env-secret")
	if _, err := LoadAWSConfig(context.Background(), "archive", "us-east-1"); err == nil {
		t.Fatal("selected missing profile silently fell back to environment credentials")
	}
}

func TestSecretEncodingRoundTrip(t *testing.T) {
	want := R2Credentials{AccessKeyID: "key", SecretAccessKey: "secret", SessionToken: "token"}
	data, err := EncodeSecret(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeSecret(data)
	if err != nil || got != want {
		t.Fatalf("decoded = %#v, err = %v", got, err)
	}
}
