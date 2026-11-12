# agent-capacity

`agent-capacity` reads plan usage windows for Claude Code, Codex, and Antigravity from passive local sources and exports them as OpenTelemetry metrics. At startup, the registry includes the Claude and Antigravity status-line sources and adds the Codex rollout source when `--codex-home` or `CODEX_HOME` supplies a home directory. `--claude-transcripts` or `AGENT_CAPACITY_CLAUDE_TRANSCRIPTS` adds the Claude transcript source.

Supported platforms include Linux (amd64 and arm64) and macOS (arm64).

```sh
bazelisk build //cmd/agent-capacity
bazelisk run //cmd/agent-capacity -- version
bazelisk run //cmd/agent-capacity -- serve --interval=60s --spool-dir=/path/to/spool
```

`serve` requires a spool directory through `--spool-dir` or `AGENT_CAPACITY_SPOOL_DIR`.

## Sources

- Claude Code reads captured `rate_limits` windows with vendor `anthropic` and source `claude_statusline`.
- Claude transcripts read local `rate_limit_event` lines with vendor `anthropic` and source `claude_transcript`.
- Antigravity reads captured `quota` windows with vendor `google` and source `agy_statusline`.
- Codex reads rollout usage events when `--codex-home` or `CODEX_HOME` supplies a home directory, with vendor `openai` and source `codex_rollout`.

Both status-line sources read `five_hour` and `seven_day` windows with `used_percentage` and Unix seconds in `resets_at`. Missing or null windows contribute no metrics. The spool stores each account alias and capture timestamp in a separate file per vendor and account, such as `agy-work.json`, so an invalid snapshot doesn't block other accounts. Status-line readings remain fresh through 10 minutes, then contribute metrics with freshness `carried` through 6 hours. Older readings contribute no metrics.

## Capture and serve

Capture an Antigravity quota snapshot, then serve from the same spool:

```sh
export AGENT_CAPACITY_SPOOL_DIR=/path/to/spool
printf '%s\n' '{"quota":{"five_hour":{"used_percentage":25,"resets_at":1794528000}}}' |
  agent-capacity statusline --vendor=agy --account=work
agent-capacity serve
```

The gauges `afakt.capacity.window.utilization`, `afakt.capacity.window.reset_time`, and `afakt.capacity.snapshot.age` include the vendor, source, and account alias labels.

## Claude transcripts

Pass a comma separated list of files or directories to `serve --claude-transcripts=/path/to/events.jsonl,/path/to/transcripts --claude-account=work`. `AGENT_CAPACITY_CLAUDE_TRANSCRIPTS` supplies the same list when the flag is absent. The alias defaults to `primary`. Directory paths select immediate regular `*.jsonl` and `*.ndjson` files, which the source opens for reading without network access or credentials.

Tests verify synthetic JSON lines with `type: rate_limit_event` and `rate_limit_info.unifiedWindows.five_hour` or `seven_day`. Windows contain a `utilization` ratio from 0 to 1, a status and `resetsAt` or `reset_time` as Unix seconds or a timestamp such as `2026-01-02T03:04:05Z`. Window keys supply the window type. Missing or null fields contribute no window. Parsing discards all other payload fields and never logs line content. It skips malformed lines and unreadable files independently.

Each collection reads at most the last 8 MiB per file and skips lines longer than 64 KiB, excluding the newline. A tail that starts inside a line skips that first fragment. Collection selects the newest valid event per alias and window. Later input replaces earlier input with equal timestamps. Age comes from the line's `timestamp` as Unix seconds or a timestamp such as `2026-01-02T03:04:05Z`, or the file modification time when the timestamp has no value. Readings remain fresh through 10 minutes, carried through 6 hours, then contribute no metrics.

Claude Code headless `--output-format stream-json`: reported, not verified here. Fixtures establish parser support only. Tests don't verify event emission by any Claude output mode.

## Export configuration

`--interval` sets the source read interval, which defaults to 60 seconds. Control the SDK export interval through `OTEL_METRIC_EXPORT_INTERVAL`. A process interrupt or termination signal flushes pending metrics and shuts down.

The export destination comes only from standard `OTEL_*` environment variables. Set `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, and `OTEL_EXPORTER_OTLP_PROTOCOL`, or their signal specific `OTEL_EXPORTER_OTLP_METRICS_*` forms. Supported protocols are `grpc` and `http/protobuf`. By default, exports use `http/protobuf`.

`OTEL_SERVICE_NAME` overrides the default service name, `agent-capacity`. `OTEL_RESOURCE_ATTRIBUTES` supplies resource attributes. Account labels must match `^[a-z0-9][a-z0-9_-]{0,31}$`.
