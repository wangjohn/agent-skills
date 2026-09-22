package storage

import (
	"context"
	"encoding/json"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"io"
	"net/http"
	"strings"
	"testing"
)

type privacyHTTP func(*http.Request) (*http.Response, error)

func (f privacyHTTP) Do(r *http.Request) (*http.Response, error) { return f(r) }
func TestPrivacyInspection(t *testing.T) {
	for _, tc := range []struct {
		name, provider, blocks, policy, acl, want string
		requests                                  int
	}{
		{"r2", "r2", "", "", "", "not_verified", 0},
		{"unknown endpoint", "", "", "", "", "not_verified", 0},
		{"denied", "s3", "", "", "", "not_verified", 3},
		{"all blocks", "s3", "<BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls><BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>true</RestrictPublicBuckets>", "", "", "verified_private", 1},
		{"unnormalized provider", "S3 ", "<BlockPublicAcls>true</BlockPublicAcls><IgnorePublicAcls>true</IgnorePublicAcls><BlockPublicPolicy>true</BlockPublicPolicy><RestrictPublicBuckets>true</RestrictPublicBuckets>", "", "", "verified_private", 1},
		{"public policy", "s3", "<BlockPublicAcls>true</BlockPublicAcls>", "<IsPublic>true</IsPublic>", "", "public_or_risky", 2},
		{"public acl", "s3", "", "<IsPublic>false</IsPublic>", `<Grant><Grantee><URI>http://acs.amazonaws.com/groups/global/AllUsers</URI></Grantee><Permission>READ</Permission></Grant>`, "public_or_risky", 3},
		{"authenticated group", "s3", "", "", `<Grant><Grantee><URI>http://acs.amazonaws.com/groups/global/AuthenticatedUsers</URI></Grantee><Permission>READ</Permission></Grant>`, "public_or_risky", 3},
		{"incomplete private", "s3", "<BlockPublicAcls>true</BlockPublicAcls>", "<IsPublic>false</IsPublic>", "<Owner><ID>owner</ID></Owner>", "not_verified", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			client := privacyHTTP(func(r *http.Request) (*http.Response, error) {
				count++
				if r.Method != http.MethodGet {
					t.Fatalf("unexpected mutation: %s", r.Method)
				}
				body := ""
				tag := ""
				switch {
				case r.URL.Query().Has("publicAccessBlock"):
					body, tag = tc.blocks, "PublicAccessBlockConfiguration"
				case r.URL.Query().Has("policyStatus"):
					body, tag = tc.policy, "PolicyStatus"
				case r.URL.Query().Has("acl"):
					body, tag = "<AccessControlList>"+tc.acl+"</AccessControlList>", "AccessControlPolicy"
					if tc.acl == "" {
						body = ""
					}
				default:
					t.Fatalf("unexpected endpoint: %s", r.URL)
				}
				status := 200
				if body == "" {
					status = 403
					body = "<Error><Code>AccessDenied</Code><Message>secret provider details</Message></Error>"
				} else {
					body = "<" + tag + ">" + body + "</" + tag + ">"
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/xml"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test", "test", ""), HTTPClient: client}
			store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "synthetic", Provider: tc.provider})
			if err != nil {
				t.Fatal(err)
			}
			report := store.InspectPrivacy(context.Background())
			if report.State != tc.want || count != tc.requests {
				t.Fatalf("report %+v, requests %d", report, count)
			}
			if strings.Contains(report.Reason, "secret") {
				t.Fatal("provider error leaked")
			}
			if report.GuidanceURL == "" {
				t.Fatal("guidance missing")
			}
			if encoded, err := json.Marshal(report); err != nil || strings.Contains(string(encoded), "checked_at") {
				t.Fatalf("uninspected report must omit checked_at: %s %v", encoded, err)
			}
		})
	}
}
