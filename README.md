# agent-capacity

`agent-capacity` reads plan usage windows for Claude Code, Codex, and Antigravity from passive local sources and exports them as OpenTelemetry metrics. Source packages arrive in later changes. Until then, the registry starts empty.

Supported platforms include Linux (amd64 and arm64) and macOS (arm64).

```sh
bazelisk build //cmd/agent-capacity
bazelisk run //cmd/agent-capacity -- version
bazelisk run //cmd/agent-capacity -- serve --interval=60s
```

`--interval` sets the source read interval, which defaults to 60 seconds. The SDK sets the export interval through `OTEL_METRIC_EXPORT_INTERVAL`. A process interrupt or termination signal flushes pending metrics and shuts down.

The export destination comes only from standard `OTEL_*` environment variables. Set `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, and `OTEL_EXPORTER_OTLP_PROTOCOL`, or their signal specific `OTEL_EXPORTER_OTLP_METRICS_*` forms. Supported protocols are `grpc` and `http/protobuf`. The default is `http/protobuf`.

`OTEL_SERVICE_NAME` overrides the default service name, `agent-capacity`. `OTEL_RESOURCE_ATTRIBUTES` supplies resource attributes. Account labels must match `^[a-z0-9][a-z0-9_-]{0,31}$`.
