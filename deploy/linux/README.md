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
    at: "2026-11-23T17:58:47Z"
---

<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright Tony Burns -->

# Deploy the Linux exporter

The template unit runs one `agent-capacity serve` per Claude account. The instance name is the account label, and the account's environment file sets its transcript directory. Install it on each Linux host where the `agent` user runs Claude, with one instance for each `CLAUDE_CONFIG_DIR` the host's accounts use. Keep the host list and any collector credentials outside this repository.

1. Install the `linux-amd64` or `linux-arm64` binary from the release for the commit you deploy to `/usr/local/bin/agent-capacity`, and check it against `SHA256SUMS`.

2. Create `/etc/agent-capacity` and write one `<account>.env` file for each account. Set `AGENT_CAPACITY_CLAUDE_TRANSCRIPTS` to that account's `CLAUDE_CONFIG_DIR` plus `/projects`, and add the collector settings as `OTEL_` variables. Set each file to mode 600, owned by root. A directory that doesn't exist yet doesn't stop the exporter, which reports tokens once the directory appears. The instance refuses to start without its file, so it never falls back to a local collector. The unit expects an `agent` user. Change `User=` in a drop-in if the host's accounts run as another user.

3. Copy `dev.tbhb.agent-capacity.serve@.service` to `/etc/systemd/system/` and start one instance for each account.

   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable --now dev.tbhb.agent-capacity.serve@<account>.service
   ```

4. In Mimir, query `agent_capacity_window_tokens{agent_account="<account>"}` after one export interval. The first pass over a large projects directory takes several collections. A 9.3 GB directory of 1879 files took about 16 minutes on macOS and held about 120 MiB of memory.

## Undo

Stop each instance and remove the unit, then restore the previous binary if you replaced one.

```bash
sudo systemctl disable --now dev.tbhb.agent-capacity.serve@<account>.service
sudo rm /etc/systemd/system/dev.tbhb.agent-capacity.serve@.service
sudo systemctl daemon-reload
```
