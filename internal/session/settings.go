// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.tbhb.dev/agent-capacity/internal/spool"
)

const (
	safeDefaultMode = "dontAsk"
	safeBypassMode  = "disable"
	maxDepth        = 32
	settingsPath    = "config/settings.json"
)

// denyList is the one source for the refused tools.
func denyList() []string {
	return []string{
		"Agent", "Task", "Bash", "Read", "Write", "Edit", "MultiEdit", "Glob", "Grep", "LS", "NotebookEdit",
		"WebFetch", "WebSearch", "TodoWrite", "Skill", "AskUserQuestion", "EnterPlanMode", "ExitPlanMode",
		"EnterWorktree", "ExitWorktree", "ToolSearch", "mcp__*",
	}
}

// checkSettings is the launch preflight. It refuses a linked settings file and any file
// that lacks the safety permissions RenderSettings writes. Extra deny entries pass.
func checkSettings(alias *os.Root) error {
	info, err := alias.Lstat(settingsPath)
	if err != nil || !info.Mode().IsRegular() {
		return ErrArguments
	}

	data, err := alias.ReadFile(settingsPath)
	if err != nil {
		return ErrArguments
	}

	if checkKeys(json.NewDecoder(bytes.NewReader(data)), 0) != nil {
		return ErrArguments
	}

	var raw json.RawMessage
	if field(data, "permissions", &raw) != nil {
		return ErrArguments
	}

	var (
		defaultMode, bypass string
		deny                []string
	)

	err = errors.Join(
		field(raw, "defaultMode", &defaultMode),
		field(raw, "disableBypassPermissionsMode", &bypass),
		field(raw, "deny", &deny),
	)
	if err != nil || defaultMode != safeDefaultMode || bypass != safeBypassMode {
		return ErrArguments
	}

	for _, tool := range denyList() {
		if !slices.Contains(deny, tool) {
			return ErrArguments
		}
	}

	return nil
}

func field(object json.RawMessage, name string, target any) error {
	var members map[string]json.RawMessage
	if json.Unmarshal(object, &members) != nil {
		return ErrArguments
	}

	if json.Unmarshal(members[name], target) != nil {
		return ErrArguments
	}

	return nil
}

func checkKeys(dec *json.Decoder, depth int) error {
	token, err := dec.Token()
	if err != nil {
		return ErrArguments
	}

	switch token {
	case json.Delim('{'):
		if depth >= maxDepth {
			return ErrArguments
		}

		return checkObject(dec, depth+1)
	case json.Delim('['):
		if depth >= maxDepth {
			return ErrArguments
		}

		return checkArray(dec, depth+1)
	default:
		return nil
	}
}

func checkObject(dec *json.Decoder, depth int) error {
	seen := map[string]struct{}{}

	for dec.More() {
		name, err := dec.Token()
		if err != nil {
			return ErrArguments
		}

		key := strings.ToLower(fmt.Sprint(name))

		if _, duplicate := seen[key]; duplicate {
			return ErrArguments
		}

		seen[key] = struct{}{}

		if checkKeys(dec, depth) != nil {
			return ErrArguments
		}
	}

	return closeContainer(dec)
}

func checkArray(dec *json.Decoder, depth int) error {
	for dec.More() {
		if checkKeys(dec, depth) != nil {
			return ErrArguments
		}
	}

	return closeContainer(dec)
}

func closeContainer(dec *json.Decoder) error {
	_, err := dec.Token()
	if err != nil {
		return ErrArguments
	}

	return nil
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// RenderSettings returns the settings file.
func RenderSettings(vendor, account, binary, dir string) ([]byte, error) {
	if !spool.ValidAlias(account) || !filepath.IsAbs(binary) || !filepath.IsAbs(dir) {
		return nil, ErrArguments
	}

	if vendor == vendorCodex {
		return []byte("cli_auth_credentials_store = \"file\"\nmodel = \"gpt-6-luna\"\n"), nil
	}

	if vendor != vendorClaude {
		return nil, ErrArguments
	}

	command := shellQuote(
		binary,
	) + " statusline --vendor=claude " + shellQuote(
		"--account="+account,
	) + " " + shellQuote(
		"--spool-dir="+dir,
	)
	settings := map[string]any{
		"statusLine": map[string]any{"type": "command", "command": command, "padding": 0},
		"permissions": map[string]any{
			"defaultMode":                  safeDefaultMode,
			"disableBypassPermissionsMode": safeBypassMode,
			"allow":                        []string{"CronCreate", "CronDelete", "CronList"},
			"deny":                         denyList(),
		},
	}

	// Encoding strings, numbers and lists of strings can't fail.
	data, _ := json.MarshalIndent(settings, "", "  ") //nolint:errcheck,errchkjson // Encoding fixed values cannot fail.

	return append(data, '\n'), nil
}
