# Agent instructions

## What this is

`agent-capacity` is a Go service that reads plan usage windows (percent used and reset time) for Claude Code, Codex, and Antigravity from passive local sources and exports them as OpenTelemetry metrics. The export destination comes only from the standard `OTEL_*` environment variables.

`go.tbhb.dev/agent-capacity` is the Go module path. The entry point runs `serve` or `version`. At startup, the registry includes the Claude and Antigravity status-line sources and adds the Codex rollout source when `--codex-home` or `CODEX_HOME` supplies a home directory. Explicit paths from `--claude-transcripts` or `AGENT_CAPACITY_CLAUDE_TRANSCRIPTS` add the Claude transcript source.

## Layout

`cmd/agent-capacity/` holds the entry point and its tests. `internal/capacity/` defines readings and source registration. `internal/export/` observes readings through the OpenTelemetry SDK. `.go-arch-lint.yml` defines the import contracts.

`tools/` holds the workspace status scripts and lint rules. `docs/` is an OKF bundle with its own rules in `docs/AGENTS.md`. `.github/` holds workflows, CODEOWNERS, and the repository ruleset.

## Commands

Bazel pins every tool and runs every check. [mise](https://mise.jdx.dev) installs only `bazelisk` and `uv`, at the versions `mise.toml` and `mise.lock` pin. A fresh worktree needs `mise install`, then `bazelisk run //:bazel_env` to build the tool shims. Keep the clone’s existing hooks.

| Command | Purpose |
| --- | --- |
| `bazelisk test //...` | Run every check |
| `bazelisk query 'tests(//...)'` | List the checks |
| `bazelisk run //:format` | Format the tree in place |
| `bazelisk run //:buildifier` | Format the Bazel files |
| `bazelisk run //:gazelle` | Regenerate the `BUILD.bazel` files from the Go sources |
| `bazelisk mod tidy` | Update `use_repo` in `MODULE.bazel` |
| `bazelisk build --config=release //cmd/agent-capacity` | Build the binary |
| `bazelisk build --config=release //container:image_multiarch` | Build both Linux container images without publishing |
| `bazelisk run //cmd/agent-capacity -- <flags>` | Run the entry point |

Each check runs by its target, such as `bazelisk test //:vale_test`. The Go checks are `//:golangci_lint_test`, `//:go_deadcode_test`, `//:go_arch_lint_test`, `//:go_mod_tidy_test`, `//:govulncheck_test`, `//:gomutants_test` and one `gobco_test` per package. The `nogo` analyzers also run during each Go build, and a finding fails it.

## Go toolchain

`go.mod` declares the `go` and `toolchain` lines, and Bazel's Go SDK follows the `toolchain` line with `GOTOOLCHAIN=local`. Renovate leaves both lines alone. Raise them by hand when a tool needs a newer one.

Run Go commands through `.bazel_env/bin/go` so they use the pinned SDK. Edit imports and dependencies in `go.mod`, then run Gazelle and `bazelisk mod tidy`, and commit `go.sum` and `MODULE.bazel.lock` with them. Bazel generates `MODULE.bazel.lock`, so never edit it by hand.

## Coverage and mutation testing

Each Go package has a `gobco_test` that demands 100% condition coverage. The target has no floor and no exclusion list. A new branch needs a test that takes both outcomes. `gomutants_test` fails while the tests leave a mutant alive. Account for a mutant that behaves like the original with a reviewed `// gomutants:disable-next-line <MUTATOR> reason="..."` directive.

Tests must give the same result on every run. Use fixtures for passive local inputs. Tests run offline and pass in any order.

## Conventions

Change only the files the task needs, and stage paths by name. Run `bazelisk test //...` before committing. Keep private values in an ignored file, never in the tree.

Write each Markdown paragraph on one line. `rumdl`, Vale and cspell check the Markdown and the Go comments, so keep the prose plain.

Commit messages follow Conventional Commits, as `cog.toml` configures. `cog` writes `CHANGELOG.md` from them. Use `mise run pr` and `mise run merge` for pull requests. Keep credentials and local session data out of commits.

## Identities

Use the committer identity the machine's hooks configure, signed with the machine's key. Route reviews through the reviewer identity that the machine configuration provides. Push only to a branch, and never to `main`. A real commit always runs the full hook suite.

## Workflows

`ci.yml` runs on pull requests and pushes to `main`. A `bazel` job runs `bazelisk test //...` on Linux (amd64 and arm64) and macOS (arm64). A Linux-only `container` job builds both image architectures without publishing. The final `conclusion` job requires both jobs and the repository checks to pass. `codeql.yml` analyzes Go and the workflows. `renovate.yaml` runs Renovate, and `renovate-config-validator.yaml` validates its configuration. All scheduled times use Coordinated Universal Time (UTC).
