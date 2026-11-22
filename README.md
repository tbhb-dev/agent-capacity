# agent-capacity

`agent-capacity` reads plan usage windows for Claude Code, Codex, and Antigravity from passive local sources and exports them as OpenTelemetry metrics. At startup, the registry includes the Claude status-line source and the Antigravity and Codex probe sources and adds the Codex rollout source when `--codex-home` or `CODEX_HOME` supplies a home directory. `--claude-transcripts` or `AGENT_CAPACITY_CLAUDE_TRANSCRIPTS` adds the Claude transcript source.

Supported platforms include Linux (amd64 and arm64) and macOS (arm64).

```sh
bazelisk build //cmd/agent-capacity
bazelisk run //cmd/agent-capacity -- version
bazelisk run //cmd/agent-capacity -- serve --interval=60s --spool-dir=/path/to/spool
```

`serve` requires a spool directory through `--spool-dir` or `AGENT_CAPACITY_SPOOL_DIR`.

## Container image

The `ci` workflow builds the image for Linux amd64 and arm64 on every pull request. It doesn't publish images. Build the Open Container Initiative (OCI) image layout locally with `bazelisk build --config=release //container:image_multiarch` without a container daemon.

The image contains one static Go binary with `cgo` off and uses a digest-pinned distroless static base with CA certificates for the exporter. Its entrypoint runs `agent-capacity serve`. It defaults to the non-root user `65532:65532`, `AGENT_CAPACITY_SPOOL_DIR=/spool`, and `CODEX_HOME=/codex`.

The binary target in `cmd/agent-capacity/BUILD.bazel` sets `pure = "on"`, which turns `cgo` off for every Bazel build of that binary, including builds outside the container image.

At runtime, mount the host spool at `/spool` and only the host Codex sessions folder at `/codex/sessions`, both read-only. Set the runtime user to the spool owner's numeric user and group IDs. Use `--user "$(id -u):$(id -g)"` when the current user is the spool owner. A mode-0700 spool requires its owner's user ID. Choose a nonzero user ID that also has read access to the sessions folder and its files. For user namespaces, map the runtime user ID to the host spool owner rather than choosing a fixed host user ID in the image.

Set the export destination only through the standard `OTEL_EXPORTER_OTLP_*` environment variables at runtime. The image contains no tokens, sign-in directory, or config directory. Keep those directories outside the mounts.

## Sources

- Claude Code reads captured `rate_limits` windows with vendor `anthropic` and source `claude_statusline`.
- Claude transcripts read local `rate_limit_event` lines with vendor `anthropic` and source `claude_transcript`.
- Antigravity reads the quota groups that `agent-capacity probe` captures, with vendor `google` and source `agy_statusline`.
- Codex reads probe snapshots with vendor `openai` and source `codex_probe`. It also reads rollout usage events when `--codex-home` or `CODEX_HOME` supplies a home directory, with vendor `openai` and source `codex_rollout`.

Claude status-line snapshots hold `five_hour` and `seven_day` windows with `used_percentage` and Unix seconds in `resets_at`, and missing or null windows contribute no metrics. `agent-capacity statusline --vendor=claude --account=work` stores the `rate_limits` object from the JSON that Claude Code pipes to it. It needs `--spool-dir` or `AGENT_CAPACITY_SPOOL_DIR`, and without one it doesn't store a snapshot. With otherwise valid arguments it logs `spool directory is required`, and with invalid arguments it logs `statusline capture failed`. It exits 0 in every case. It skips the write when the piped JSON has no `rate_limits` key, and it stores a `"rate_limits": null` value as null.

Each Antigravity bucket gives a `window` of `5h` or `weekly` (exported as `7d`), a `remaining_fraction` and a `reset_time`, with `agent.plan.bucket` set to `gemini` or `3p`. The source skips unknown buckets, and a snapshot without any known bucket is invalid.

The spool stores one file per vendor and account alias, named `<vendor>-<alias>.json` with vendor `claude`, `agy` or `codex`, such as `claude-work.json` or `agy-work.json`. Each file stores the vendor, the alias, the capture timestamp and a `data` object. For Antigravity, `data` is the `data` object inside the `command` object of the `agy` usage command's JSON output, which `agent-capacity probe` captures. A separate file per account means an invalid snapshot doesn't block other accounts.

Claude status-line readings remain fresh through 10 minutes, and Antigravity readings through 30 minutes. Both then contribute metrics with freshness `carried` through 6 hours. Older readings contribute no metrics.

## Capture and serve

Capture an Antigravity quota snapshot, then serve from the same spool:

```sh
export AGENT_CAPACITY_SPOOL_DIR=/path/to/spool
agent-capacity probe --account=work
agent-capacity serve
```

The probe runs `agy -p "/usage" --output-format json --print-timeout 100s` from `PATH`, a command that neither starts an agent turn nor spends quota. Run the probe as the account's user, with that account's Antigravity home, because it inherits the environment. It stores only the `data` object inside the output's `command` object and exits 1 when the command fails or reports a status other than `SUCCESS`. Run it on the reading interval, such as every 15 minutes.
Run `agent-capacity probe --vendor=codex --account=work --spool-dir=/path/to/spool` as the Codex account’s user. The probe runs `codex app-server` on standard input and output, reads `account/rateLimits/read` with a 30-second timeout, and stores only the rate limits in `codex-work.json`. The probe reads no authentication files and prints no server output. `serve --spool-dir` exports each snapshot with its account alias, window `7d` for 10080 minutes and `5h` otherwise. The vendor defaults to `agy` for existing calls.

The gauges `agent.capacity.window.utilization`, `agent.capacity.window.reset_time`, and `agent.capacity.snapshot.age` include the vendor, source, and account alias labels. Antigravity readings add `agent.plan.bucket`.

## Claude transcripts

Pass a comma separated list of files or directories to `serve --claude-transcripts=/path/to/events.jsonl,/path/to/transcripts --claude-account=work`. `AGENT_CAPACITY_CLAUDE_TRANSCRIPTS` supplies the same list when the flag is absent. The alias defaults to `primary`. Directory paths select immediate regular `*.jsonl` and `*.ndjson` files, which the source opens for reading without network access or credentials.

Tests verify synthetic JSON lines with `type: rate_limit_event` and `rate_limit_info.unifiedWindows.five_hour` or `seven_day`. Windows contain a `utilization` ratio from 0 to 1, a status and `resetsAt` or `reset_time` as Unix seconds or a timestamp such as `2026-01-02T03:04:05Z`. Window keys supply the window type. Missing or null fields contribute no window. Parsing discards all other payload fields and never logs line content. It skips malformed lines and unreadable files independently.

Named paths follow symlinks, and directory entries don't follow symlinks or recurse into subdirectories.

To find rate readings, each collection skips files with modification times older than 6 hours. It reads at most the last 8 MiB per remaining file and skips lines longer than 64 KiB, excluding the newline. A tail that starts inside a line skips that first fragment. Collection selects the newest valid event per alias and window. Later input replaces earlier input with equal timestamps. Age comes from the line's `timestamp` as Unix seconds or a timestamp such as `2026-01-02T03:04:05Z`, or the file modification time when the timestamp has no value. The source rejects events with timestamps more than 5 minutes ahead of the collection clock, including timestamps from file modification times. Readings remain fresh through 10 minutes, carried through 6 hours, then contribute no metrics.

Claude Code headless `--output-format stream-json`: reported, not verified here. Fixtures establish parser support only. Tests don't verify event emission by any Claude output mode.

## Tokens by model

The gauge `agent.capacity.window.tokens` becomes `agent_capacity_window_tokens` in Prometheus. It adds `model` and `token_type` (`input`, `output`, `cache_read`, `cache_write`) to the existing `agent.account`, `agent.plan.vendor`, `agent.plan.window`, `agent.capacity.source` and `agent.capacity.freshness` labels (dots become underscores in Prometheus). Values sum assistant usage in the configured Claude transcript files from the latest valid reset time minus 5 hours or 7 days through collection time or the reset time, whichever comes first, inclusive. Repeated `message.id` values contribute only their last usage, ordered by timestamp and then input order. No valid window reading means no token series for that window. Token series follow the reading's freshness and expiry.

Token scans read complete files with modification times at or after the window start, including files older than the 6-hour limit used to find rate readings. They share a 64 MiB byte budget per collection across both windows (5h first), separate from the rate-reading tails. Repeated paths are scanned once per window. Lines of any length up to 32 MiB are parsed. A longer line is skipped and the scan continues. A file exceeding the remaining budget or a read failure discards that window's entire token sum. A window that fits can still export when the other exceeds a limit. Only event type, model, message ID, timestamp and the four usage counts are decoded and retained during collection; message content is never retained, logged or exported.

## Session runner

The session runner starts Claude Code with a fixed deny list of tools and the `dontAsk` permission mode. Each launch first checks the Claude settings file. A file passes only with the rendered keys, the rendered `allow` list and status line command, and every refused tool in `deny`, with `padding` left unchecked. Extra `deny` entries pass, because they only restrict more. This floor guards against a stale or mis-rendered file and doesn't defend against a hostile writer. Since the deny list is a snapshot of tool names and `mcp__*` wildcard support in `deny` and the vendor's permission precedence are unverified, `dontAsk` is the real control. Codex's config isn't checked, and it has no tool deny list. The runner starts `codex exec` with `--sandbox read-only` and the `never` approval policy, and those flags are the control.

A Claude run and `session render-settings` require `--exporter`, the absolute path of the agent-capacity binary that the status line runs. `--binary` names only the vendor binary. A Claude run without `--exporter` refuses to start with exit 1, while a relative path or a missing identity exits 2.

Cancelling a login signals the login child and then stops its descendants. The runner lists descendants for as long as the login child runs, with a last listing before the signal. Descendants of a login child that fails on its own get the same cleanup, and one whose process ID now shows another kernel start time stays alone. Because the child shares the caller's terminal, it can't have its own process group, and anything it starts after the last listing keeps running. Session children do run in their own group. For those, the runner lists descendants of the child and of every descendant it already recorded, with `ps -A` runs every tenth of a second for the first two seconds and then every half second, which settles at two `ps -A` forks per second for each child, and, once the child ends on its own or on a stop, stops each one that still shows its recorded start time. Start times come from the kernel, to the microsecond on macOS and to the clock tick on Linux. A reused process ID shows another start time and stays alone. A stop during that cleanup waits for it. Between two listings, a descendant can start and leave the group unseen.

## Export configuration

`--interval` sets the source read interval, which defaults to 60 seconds. Control the SDK export interval through `OTEL_METRIC_EXPORT_INTERVAL`. A process interrupt or termination signal flushes pending metrics and shuts down.

The export destination comes only from standard `OTEL_*` environment variables. Set `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, and `OTEL_EXPORTER_OTLP_PROTOCOL`, or their signal specific `OTEL_EXPORTER_OTLP_METRICS_*` forms. Supported protocols are `grpc` and `http/protobuf`. By default, exports use `http/protobuf`.

`OTEL_SERVICE_NAME` overrides the default service name, `agent-capacity`. `OTEL_RESOURCE_ATTRIBUTES` supplies resource attributes. Account labels must match `^[a-z0-9][a-z0-9_-]{0,31}$`.
