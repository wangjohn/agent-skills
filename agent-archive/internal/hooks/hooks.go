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
		events = []string{"SessionStart", "UserPromptSubmit", "Stop", "Interrupt", "SessionEnd", "SubagentStop"}
	case "claude":
		events = []string{"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "SubagentStop"}
	case "cursor":
		events = []string{"sessionStart", "beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionEnd", "subagentStop"}
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
	if _, err := stripOwned(hs, harness); err != nil {
		return nil, err
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

// Remove strips every handler this tool installed for harness from existing,
// leaving unrelated handlers and top-level settings exactly as they were. It
// reports whether anything was actually removed so a caller can skip
// rewriting a file that never contained our entries. Like Merge, it only
// ever matches our own marker (or the exact known prototype handler), never
// a substring of an unrelated command.
func Remove(existing []byte, harness string) ([]byte, bool, error) {
	switch harness {
	case "codex", "claude", "cursor":
	default:
		return nil, false, errors.New("unsupported harness")
	}
	if len(bytes.TrimSpace(existing)) == 0 {
		return existing, false, nil
	}
	var root map[string]any
	if err := json.Unmarshal(existing, &root); err != nil || root == nil {
		return nil, false, errors.New("invalid existing hook configuration")
	}
	hs, ok := root["hooks"].(map[string]any)
	if !ok {
		if root["hooks"] != nil {
			return nil, false, errors.New("invalid hooks object")
		}
		return existing, false, nil
	}
	removed, err := stripOwned(hs, harness)
	if err != nil {
		return nil, false, err
	}
	if !removed {
		return existing, false, nil
	}
	// An event whose only handlers were ours is dropped entirely rather than
	// left as an empty list setup never created.
	for event, raw := range hs {
		if list, ok := raw.([]any); ok && len(list) == 0 {
			delete(hs, event)
		}
	}
	data, err := json.MarshalIndent(root, "", "  ")
	return append(data, '\n'), true, err
}

// stripOwned removes only our marker or an exact known prototype handler
// from every event in hs, including events no longer used by the current
// implementation. It never removes by substring alone. It reports whether
// any handler was removed.
func stripOwned(hs map[string]any, harness string) (bool, error) {
	removed := false
	for event, raw := range hs {
		groups, ok := raw.([]any)
		if !ok {
			return false, fmt.Errorf("invalid hook list for %s", event)
		}
		kept := []any{}
		for _, item := range groups {
			g, ok := item.(map[string]any)
			if !ok {
				return false, errors.New("invalid hook entry")
			}
			if harness == "cursor" {
				if old, ok := g["command"].(string); ok && strings.HasSuffix(old, " # "+Owner) {
					removed = true
					continue
				}
				kept = append(kept, g)
				continue
			}
			handlers, ok := g["hooks"].([]any)
			if !ok {
				return false, errors.New("invalid hook handlers")
			}
			remaining := []any{}
			for _, h := range handlers {
				m, ok := h.(map[string]any)
				if !ok {
					return false, errors.New("invalid hook handler")
				}
				if m["statusMessage"] == Owner || m["statusMessage"] == "Recording private skill-run evidence" {
					removed = true
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
	return removed, nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
