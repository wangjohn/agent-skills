// Package evidence produces bounded, privacy-filtered observations which are
// kept separate from an application's native transcript.
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
)

const (
	maxSkillsPerRoot = 256
	maxSkillBytes    = 1 << 20
)

// SkillOptions identifies the only filesystem locations a background
// collection pass may inspect. ProjectRoot is already an explicitly included
// project and UserHome is the local user's home; no repository-wide walk is
// performed.
type SkillOptions struct {
	Harness     string
	ProjectRoot string
	UserHome    string
	ObservedAt  time.Time
}

type skillRoot struct {
	path  string
	scope string
}

// ObserveSkills inventories immediate SKILL.md children of documented skill
// roots. Filesystem presence proves installation only. It deliberately does
// not claim that the harness discovered, exposed, or invoked a skill in this
// session. The returned evidence has already passed the archive privacy
// filter and is safe to place in a local upload request or source candidate.
func ObserveSkills(options SkillOptions) ([]archive.SupplementalEvidence, error) {
	if options.ObservedAt.IsZero() {
		return nil, errors.New("skill observation time is required")
	}
	roots := skillRoots(options)
	var observations []archive.SupplementalEvidence
	for _, root := range roots {
		evidence, err := observeRoot(options.Harness, root, options.ObservedAt)
		if err != nil {
			return nil, err
		}
		observations = append(observations, evidence...)
	}
	filtered, _, err := archive.FilterSupplementalEvidence(observations)
	if err != nil {
		return nil, fmt.Errorf("filter skill evidence: %w", err)
	}
	return filtered, nil
}

func skillRoots(options SkillOptions) []skillRoot {
	project, user := filepath.Clean(options.ProjectRoot), filepath.Clean(options.UserHome)
	if options.ProjectRoot == "" {
		project = ""
	}
	if options.UserHome == "" {
		user = ""
	}
	var roots []skillRoot
	add := func(base, suffix, scope string) {
		if base != "" {
			roots = append(roots, skillRoot{path: filepath.Join(base, suffix), scope: scope})
		}
	}
	switch strings.ToLower(strings.TrimSpace(options.Harness)) {
	case "codex":
		add(user, ".agents/skills", "user")
		add(project, ".agents/skills", "project")
	case "claude", "claude-code":
		add(user, ".claude/skills", "user")
		add(project, ".claude/skills", "project")
	case "cursor":
		add(user, ".cursor/skills", "user")
		add(project, ".cursor/skills", "project")
	}
	return roots
}

func observeRoot(harness string, root skillRoot, observedAt time.Time) ([]archive.SupplementalEvidence, error) {
	entries, err := os.ReadDir(root.path)
	if errors.Is(err, os.ErrNotExist) {
		// Absence is not evidence that no skills were available. Emit nothing
		// and leave coverage unknown.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s skill inventory: %w", root.scope, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if len(entries) > maxSkillsPerRoot {
		entries = entries[:maxSkillsPerRoot]
	}
	provenance := "filesystem:" + strings.ToLower(strings.TrimSpace(harness))
	var inventory []any
	var snapshots []archive.SupplementalEvidence
	for _, entry := range entries {
		path := filepath.Join(root.path, entry.Name(), "SKILL.md")
		info, err := os.Stat(path) // follows supported skill-directory symlinks
		if errors.Is(err, os.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect %s skill %q: %w", root.scope, entry.Name(), err)
		}
		name := entry.Name()
		payload := map[string]any{
			"name": name, "coverage": string(archive.SkillCoverageInstalledOnly), "scope": root.scope,
			"uncertainty": "filesystem presence does not prove discovery, eligibility, or invocation",
		}
		if info.Size() > maxSkillBytes {
			payload["uncertainty"] = "instruction snapshot omitted because the source exceeded the size limit"
			inventory = append(inventory, map[string]any{"name": name})
			snapshots = append(snapshots, archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillSnapshot, ObservedAt: observedAt, Provenance: provenance, Payload: payload})
			continue
		}
		original, err := readBounded(path, maxSkillBytes)
		if err != nil {
			return nil, fmt.Errorf("read %s skill %q: %w", root.scope, entry.Name(), err)
		}
		if parsed := frontmatterName(original); parsed != "" {
			name = parsed
			payload["name"] = name
		}
		digest := sha256.Sum256(original)
		hash := hex.EncodeToString(digest[:])
		payload["sha256"] = hash // hash of original bytes, before filtering
		payload["snapshot"] = string(original)
		payload["redacted"] = false
		filtered, gaps, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{{
			Kind: archive.EvidenceKindSkillSnapshot, ObservedAt: observedAt, Provenance: provenance, Payload: payload,
		}})
		if err != nil {
			return nil, err
		}
		if len(filtered) == 0 {
			continue
		}
		if len(gaps) > 0 {
			filtered[0].Payload["redacted"] = true
		}
		inventory = append(inventory, map[string]any{"name": name, "sha256": hash})
		snapshots = append(snapshots, filtered[0])
	}
	if len(inventory) == 0 {
		// An empty directory does not establish that the harness had no
		// bundled, synced, managed, or otherwise non-filesystem skills.
		return nil, nil
	}
	result := []archive.SupplementalEvidence{{
		Kind: archive.EvidenceKindSkillInventory, ObservedAt: observedAt, Provenance: provenance,
		Payload: map[string]any{
			"coverage": string(archive.SkillCoverageInstalledOnly), "scope": root.scope, "skills": inventory,
			"uncertainty": "filesystem presence does not prove discovery or eligibility in this session",
		},
	}}
	return append(result, snapshots...), nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("source exceeded size limit while reading")
	}
	return data, nil
}

func frontmatterName(content []byte) string {
	lines := strings.Split(string(content), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	for _, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "---" {
			break
		}
		if !strings.HasPrefix(strings.ToLower(trimmed), "name:") {
			continue
		}
		name := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, trimmed[:5])), "\"'")
		if name != "" && len(name) <= 256 && !strings.ContainsAny(name, "\r\n\x00") {
			return name
		}
	}
	return ""
}
