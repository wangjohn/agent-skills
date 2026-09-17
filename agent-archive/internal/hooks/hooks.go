// Package hooks owns lifecycle configuration only. It never trusts hooks or runs an agent.
package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const Owner = "agent-archive lifecycle capture"

// Merge preserves unrelated handlers and top-level settings. The executable must
// be the installed absolute path, not a developer checkout or shell fragment.
func Merge(existing []byte, harness, executable string) ([]byte, error) {
	if !filepath.IsAbs(executable) || strings.ContainsAny(executable, "\x00\r\n") {
		return nil, errors.New("executable must be an absolute path")
	}
	var root map[string]any
	if len(bytes.TrimSpace(existing)) == 0 {
		root = map[string]any{}
	} else if err := json.Unmarshal(existing, &root); err != nil || root == nil {
		return nil, errors.New("invalid existing hook configuration")
	}
	var events []string
	switch harness {
	case "codex":
		events = []string{"SessionStart", "Stop", "Interrupt", "SessionEnd", "SubagentStop"}
	case "claude":
		events = []string{"SessionStart", "Stop", "StopFailure", "SessionEnd", "SubagentStop"}
	case "cursor":
		events = []string{"sessionStart", "stop", "sessionEnd", "subagentStop"}
		if v, ok := root["version"]; ok && v != float64(1) {
			return nil, errors.New("unsupported Cursor hook configuration version")
		}
		root["version"] = 1
	default:
		return nil, errors.New("unsupported harness")
	}
	hs, ok := root["hooks"].(map[string]any)
	if !ok {
		if root["hooks"] != nil {
			return nil, errors.New("invalid hooks object")
		}
		hs = map[string]any{}
		root["hooks"] = hs
	}
	command := quote(executable) + " _hook --harness " + harness + " # " + Owner
	// Remove only our marker or an exact known prototype handler, including events
	// no longer used by the new implementation. Never remove by substring alone.
	for event, raw := range hs {
		groups, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("invalid hook list for %s", event)
		}
		kept := []any{}
		for _, item := range groups {
			g, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("invalid hook entry")
			}
			if harness == "cursor" {
				if old, ok := g["command"].(string); ok && strings.HasSuffix(old, " # "+Owner) {
					continue
				}
				kept = append(kept, g)
				continue
			}
			handlers, ok := g["hooks"].([]any)
			if !ok {
				return nil, errors.New("invalid hook handlers")
			}
			remaining := []any{}
			for _, h := range handlers {
				m, ok := h.(map[string]any)
				if !ok {
					return nil, errors.New("invalid hook handler")
				}
				if m["statusMessage"] == Owner || m["statusMessage"] == "Recording private skill-run evidence" {
					continue
				}
				remaining = append(remaining, h)
			}
			if len(remaining) > 0 {
				g["hooks"] = remaining
				kept = append(kept, g)
			}
		}
		hs[event] = kept
	}
	for _, event := range events {
		handler := map[string]any{"command": command, "timeout": 2}
		var entry any = handler
		if harness != "cursor" {
			handler["type"] = "command"
			handler["statusMessage"] = Owner
			entry = map[string]any{"hooks": []any{handler}}
		}
		list, _ := hs[event].([]any)
		hs[event] = append(list, entry)
	}
	data, err := json.MarshalIndent(root, "", "  ")
	return append(data, '\n'), err
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
