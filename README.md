# agent-capacity

`agent-capacity` is a Go service that reads plan usage windows (percent used and reset time) for Claude Code, Codex, and Antigravity from passive local sources and exports them as OpenTelemetry metrics. The export destination comes only from the standard `OTEL_*` environment variables. The current binary prints usage and exits with status 0.

```sh
bazelisk build //cmd/agent-capacity
```
