---
description: Install, verify, and remove the Linux exporter unit.
status: stable
tags: [deployment, linux]
title: Deploy the Linux exporter
type: How-to
generated:
  - role: generated
    harness: claude-code:2.1.322
    model: anthropic:claude-sonnet-5-5
    effort: medium
    surface: cli
    session: sha-256-64:1e5db78070fe05b4
    at: "2026-11-23T17:37:48Z"
---

<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright Tony Burns -->

# Deploy the Linux exporter

The unit runs `agent-capacity serve` as the `agent` user and exports the token series for the Claude transcripts under that user's home. Install it on each Linux host where the `agent` user runs Claude. Keep the host list and any collector credentials outside this repository.

1. Install the `linux-amd64` or `linux-arm64` binary from the release for the commit you deploy to `/usr/local/bin/agent-capacity`, and check it against `SHA256SUMS`.

2. Write the collector settings to `/etc/agent-capacity/serve.env` as `OTEL_` variables. Set the file to mode 600, owned by root.

3. Copy `dev.tbhb.agent-capacity.serve.service` to `/etc/systemd/system/` and start it.

   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable --now dev.tbhb.agent-capacity.serve.service
   ```

4. In Mimir, query `agent_capacity_window_tokens{agent_account="claude-agent"}` after one export interval. The first pass over a large projects directory takes several collections. A 9.3 GB directory of 1879 files took about 16 minutes on macOS and held about 120 MiB of memory.

## Undo

Stop and remove the unit, then restore the previous binary if you replaced one.

```bash
sudo systemctl disable --now dev.tbhb.agent-capacity.serve.service
sudo rm /etc/systemd/system/dev.tbhb.agent-capacity.serve.service
sudo systemctl daemon-reload
```
