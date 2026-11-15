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
	// maxSettingsBytes bounds the settings read. The rendered file is about 1 KiB.
	maxSettingsBytes = 64 << 10
	settingsPath     = "config/settings.json"
	commandKey       = "command"
)

// allowList is the one source for the pre-approved tools.
func allowList() []string { return []string{"CronCreate", "CronDelete", "CronList"} }

// denyList is the one source for the refused tools.
func denyList() []string {
	return []string{
		"Agent", "Task", "Bash", "Read", "Write", "Edit", "MultiEdit", "Glob", "Grep", "LS", "NotebookEdit",
		"WebFetch", "WebSearch", "TodoWrite", "Skill", "AskUserQuestion", "EnterPlanMode", "ExitPlanMode",
		"EnterWorktree", "ExitWorktree", "ToolSearch", "mcp__*",
	}
}

// checkSettings is the launch preflight for the Claude settings file. It refuses a linked file or `config`
// directory, duplicate or
// case-variant keys, and any file that differs from what RenderSettings writes in its keys, its `allow` list and its
// `statusLine` command, which must equal command. The top level accepts only `statusLine` and `permissions`. The
// `permissions` object accepts only `defaultMode`, `disableBypassPermissionsMode`, `allow` and `deny`. The
// `statusLine` object accepts only `type`, `command` and `padding`, and it doesn't check `padding`. The `deny` list must include every refused tool,
// and extra string entries pass. The check is a floor against a stale or mis-rendered file and doesn't defend against
// a hostile writer. It doesn't check Codex's configuration, and deny support for the `mcp__*` wildcard is
// unverified. The `dontAsk` mode is the real control.
func checkSettings(alias *os.Root, command string) error {
	// The startup check of the directory ran earlier, so a link made since then is caught here.
	if realDirectory(alias, "config", true) != nil {
		return ErrArguments
	}

	data, err := spool.ReadBounded(alias, settingsPath, maxSettingsBytes)
	// gomutants:disable-next-line BRANCH_IF reason="Empty data also fails the key check."
	if err != nil {
		return ErrArguments
	}

	if checkKeys(json.NewDecoder(bytes.NewReader(data)), 0) != nil {
		return ErrArguments
	}

	if onlyKeys(data, "statusLine", "permissions") != nil {
		return ErrArguments
	}

	var permissions, statusLine json.RawMessage

	// gomutants:disable-next-line BRANCH_IF reason="An empty member also fails the checks that follow."
	if errors.Join(field(data, "permissions", &permissions), field(data, "statusLine", &statusLine)) != nil {
		return ErrArguments
	}

	if checkStatusLine(statusLine, command) != nil || checkPermissions(permissions) != nil {
		return ErrArguments
	}

	return nil
}

func checkStatusLine(statusLine json.RawMessage, command string) error {
	var kind, got string

	err := errors.Join(
		onlyKeys(statusLine, "type", commandKey, "padding"),
		field(statusLine, "type", &kind),
		field(statusLine, commandKey, &got),
	)
	if err != nil || kind != commandKey || got != command {
		return ErrArguments
	}

	return nil
}

func checkPermissions(permissions json.RawMessage) error {
	var (
		defaultMode, bypass string
		allow               []string
		deny                []*string
	)

	err := errors.Join(
		onlyKeys(permissions, "defaultMode", "disableBypassPermissionsMode", "allow", "deny"),
		field(permissions, "defaultMode", &defaultMode),
		field(permissions, "disableBypassPermissionsMode", &bypass),
		field(permissions, "allow", &allow),
		field(permissions, "deny", &deny),
	)
	if err != nil || defaultMode != safeDefaultMode || bypass != safeBypassMode {
		return ErrArguments
	}

	if !slices.Equal(allow, allowList()) {
		return ErrArguments
	}

	// gomutants:disable-next-line INTEGER_INCREMENT reason="The capacity only sizes the slice."
	names := make([]string, 0, len(deny))

	for _, tool := range deny {
		if tool == nil {
			return ErrArguments
		}

		names = append(names, *tool)
	}

	for _, tool := range denyList() {
		if !slices.Contains(names, tool) {
			return ErrArguments
		}
	}

	return nil
}

// onlyKeys refuses a JSON object that has a member outside allowed.
func onlyKeys(object json.RawMessage, allowed ...string) error {
	var members map[string]json.RawMessage
	// gomutants:disable-next-line BRANCH_IF reason="The field lookups that follow fail the same way."
	if json.Unmarshal(object, &members) != nil {
		// gomutants:disable-next-line RETURN_ERROR_NIL reason="The field lookups that follow fail the same way."
		return ErrArguments
	}

	for name := range members {
		if !slices.Contains(allowed, name) {
			return ErrArguments
		}
	}

	return nil
}

func field(object json.RawMessage, name string, target any) error {
	var members map[string]json.RawMessage
	// gomutants:disable-next-line BRANCH_IF reason="The same failure follows."
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

		key := spool.FoldKey(fmt.Sprint(name))

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

	// gomutants:disable-next-line RETURN_ERROR_NIL reason="json.Unmarshal rejects the same input."
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

// statusCommand is the status line command that RenderSettings writes.
func statusCommand(binary, account, dir string) string {
	return shellQuote(binary) + " statusline --vendor=claude " + shellQuote("--account="+account) + " " +
		shellQuote("--spool-dir="+dir)
}

// RenderSettings returns the settings file.
func RenderSettings(vendor, account, binary, dir string) ([]byte, error) {
	if !spool.ValidAlias(account) || !filepath.IsAbs(binary) || !filepath.IsAbs(dir) {
		return nil, ErrArguments
	}

	if vendor == vendorCodex {
		return []byte("cli_auth_credentials_store = \"file\"\nmodel = \"gpt-6.1-sol\"\n"), nil
	}

	if vendor != vendorClaude {
		return nil, ErrArguments
	}

	command := statusCommand(binary, account, dir)
	settings := map[string]any{
		"statusLine": map[string]any{"type": commandKey, commandKey: command, "padding": 0},
		"permissions": map[string]any{
			"defaultMode":                  safeDefaultMode,
			"disableBypassPermissionsMode": safeBypassMode,
			"allow":                        allowList(),
			"deny":                         denyList(),
		},
	}

	// Encoding strings, numbers and lists of strings can't fail.
	data, _ := json.MarshalIndent(settings, "", "  ") //nolint:errcheck,errchkjson // Encoding fixed values cannot fail.

	return append(data, '\n'), nil
}
