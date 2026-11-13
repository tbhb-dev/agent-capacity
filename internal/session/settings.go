// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.tbhb.dev/agent-capacity/internal/spool"
)

const (
	safeDefaultMode = "dontAsk"
	safeBypassMode  = "disable"
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

	var settings struct {
		Permissions struct {
			DefaultMode string   `json:"defaultMode"`
			Bypass      string   `json:"disableBypassPermissionsMode"`
			Deny        []string `json:"deny"`
		} `json:"permissions"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return ErrArguments
	}

	permissions := settings.Permissions
	if permissions.DefaultMode != safeDefaultMode || permissions.Bypass != safeBypassMode {
		return ErrArguments
	}

	for _, tool := range denyList() {
		if !slices.Contains(permissions.Deny, tool) {
			return ErrArguments
		}
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
