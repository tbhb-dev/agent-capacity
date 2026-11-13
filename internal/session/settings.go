// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package session

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"go.tbhb.dev/agent-capacity/internal/spool"
)

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// RenderSettings returns a template. A real run must check the vendor permission keys.
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
			"defaultMode":                  "dontAsk",
			"disableBypassPermissionsMode": "disable",
			"allow":                        []string{"CronCreate", "CronDelete", "CronList"},
			"deny": []string{
				"Agent",
				"Task",
				"Bash",
				"Read",
				"Write",
				"Edit",
				"MultiEdit",
				"Glob",
				"Grep",
				"LS",
				"NotebookEdit",
				"WebFetch",
				"WebSearch",
				"TodoWrite",
				"Skill",
				"AskUserQuestion",
				"EnterPlanMode",
				"ExitPlanMode",
				"EnterWorktree",
				"ExitWorktree",
				"ToolSearch",
				"mcp__*",
			},
		},
	}

	// Encoding strings, numbers and lists of strings can't fail.
	data, _ := json.MarshalIndent(settings, "", "  ") //nolint:errcheck,errchkjson // Encoding fixed values cannot fail.

	return append(data, '\n'), nil
}
