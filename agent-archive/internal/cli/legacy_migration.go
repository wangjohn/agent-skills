package cli

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-skills/agent-archive/internal/hooks"
)

const legacyLaunchLabel = "com.agent-skills.skill-runs-upload"

type legacyJob struct {
	Change    hooks.Change `json:"change"`
	WasLoaded bool         `json:"was_loaded"`
}

// Only the prototype's exact label and command shape establish ownership.
// Do not execute the plist or remove the private records referenced by --home.
func planLegacyMigration(userHome string, env Env) (*legacyJob, error) {
	path := filepath.Join(userHome, "Library", "LaunchAgents", legacyLaunchLabel+".plist")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var key, label string
	var args []string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("cannot verify legacy upload job ownership: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "key":
			if err := decoder.DecodeElement(&key, &start); err != nil {
				return nil, err
			}
		case "string":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return nil, err
			}
			if key == "Label" {
				label = value
			}
			key = ""
		case "array":
			if key == "ProgramArguments" {
				var a struct {
					Values []string `xml:"string"`
				}
				if err := decoder.DecodeElement(&a, &start); err != nil {
					return nil, err
				}
				args = a.Values
				key = ""
			}
		}
	}
	if label != legacyLaunchLabel || len(args) != 5 || filepath.Base(args[1]) != "skill_runs.py" || args[2] != "--home" || args[3] == "" || args[4] != "upload" {
		return nil, fmt.Errorf("legacy job path contains an unrecognized command; preserve %s and resolve it before setup", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	state := env.jobState(path)
	if state == "unknown" {
		return nil, fmt.Errorf("cannot determine legacy upload job state; restore launchctl access and retry")
	}
	return &legacyJob{Change: hooks.Change{Path: path, Before: data, Existed: true, Mode: info.Mode().Perm()}, WasLoaded: state == "loaded" || state == "running"}, nil
}

func retireLegacyJob(job *legacyJob, env Env) error {
	if job == nil {
		return nil
	}
	current, err := os.ReadFile(job.Change.Path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, job.Change.Before) {
		return fmt.Errorf("legacy upload job changed during setup; retry")
	}
	if job.WasLoaded {
		if err := env.unloadLaunchAgent(job.Change.Path); err != nil {
			return err
		}
	}
	return os.Remove(job.Change.Path)
}

func restoreLegacyJob(job *legacyJob, env Env) error {
	if job == nil {
		return nil
	}
	current, err := os.ReadFile(job.Change.Path)
	if os.IsNotExist(err) {
		if err := hooks.Apply([]hooks.Change{{Path: job.Change.Path, After: job.Change.Before, Mode: job.Change.Mode}}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !bytes.Equal(current, job.Change.Before) {
		return fmt.Errorf("legacy upload job changed outside setup; preserve it and resolve recovery")
	}
	if job.WasLoaded {
		state := env.jobState(job.Change.Path)
		if state == "unknown" {
			return fmt.Errorf("legacy job state is unknown; retry recovery when launchctl is available")
		}
		if state != "loaded" && state != "running" {
			return env.loadLaunchAgent(job.Change.Path)
		}
	}
	return nil
}
