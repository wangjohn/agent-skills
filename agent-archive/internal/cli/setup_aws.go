package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
)

// AWSProfile contains only the information needed to offer a profile in setup.
// Discovery never retrieves credentials, runs credential_process, or logs in.
type AWSProfile struct{ Name, Region string }

func (e Env) awsProfiles() ([]AWSProfile, error) {
	if e.AWSProfiles != nil {
		return e.AWSProfiles()
	}
	home, err := e.userHomeDir()
	if err != nil {
		return nil, err
	}
	configPath := firstNonEmpty(os.Getenv("AWS_CONFIG_FILE"), filepath.Join(home, ".aws", "config"))
	credentialsPath := firstNonEmpty(os.Getenv("AWS_SHARED_CREDENTIALS_FILE"), filepath.Join(home, ".aws", "credentials"))
	return readAWSProfiles(configPath, credentialsPath)
}

func readAWSProfiles(configPath, credentialsPath string) ([]AWSProfile, error) {
	names := map[string]bool{}
	for i, path := range []string{configPath, credentialsPath} {
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read AWS profile settings")
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			// Only section names are used; values may contain credentials and are ignored.
			if !strings.HasPrefix(line, "[") {
				continue
			}
			end := strings.Index(line, "]")
			if end < 0 {
				continue
			}
			name := strings.TrimSpace(line[1:end])
			if i == 0 {
				if name != "default" && !strings.HasPrefix(name, "profile ") {
					continue
				}
				name = strings.TrimSpace(strings.TrimPrefix(name, "profile "))
			}
			if name != "" {
				names[name] = true
			}
		}
		scanErr := scanner.Err()
		f.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("cannot read AWS profile settings")
		}
	}
	profiles := make([]AWSProfile, 0, len(names))
	for name := range names {
		cfg, err := awsconfig.LoadSharedConfigProfile(context.Background(), name, func(o *awsconfig.LoadSharedConfigOptions) {
			o.ConfigFiles = []string{configPath}
			o.CredentialsFiles = []string{credentialsPath}
		})
		region := ""
		if err == nil {
			region = cfg.Region
		}
		profiles = append(profiles, AWSProfile{Name: name, Region: region})
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

func promptAWSProfile(p *prompter, cfg *credentials.Config, env Env) error {
	profiles, err := env.awsProfiles()
	if err != nil {
		fmt.Fprintln(p.out, "Could not read AWS profiles automatically. Enter an existing profile name below.")
	}
	names := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		names = append(names, profile.Name)
	}
	if len(names) > 0 {
		fmt.Fprintf(p.out, "AWS profiles: %s\n", strings.Join(names, ", "))
	}
	def := cfg.AWSProfile
	if def == "" {
		if containsString(names, "default") {
			def = "default"
		} else if len(names) == 1 {
			def = names[0]
		}
	}
	profile, err := p.required("AWS profile", def)
	if err != nil {
		return err
	}
	if profile != cfg.AWSProfile {
		cfg.Region = ""
	}
	cfg.AWSProfile = profile
	if cfg.Region == "" {
		for _, candidate := range profiles {
			if candidate.Name == profile {
				cfg.Region = candidate.Region
				break
			}
		}
	}
	if cfg.Region == "" {
		cfg.Region, err = p.required("Bucket region (for example us-east-1)", "")
		return err
	}
	fmt.Fprintf(p.out, "Using region %s. You can change it at the final review.\n", cfg.Region)
	return nil
}
