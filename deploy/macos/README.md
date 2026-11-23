---
description: Build, install, verify, and roll back the macOS exporters.
status: stable
tags: [deployment, macos]
title: Deploy the macOS exporters
type: How-to
generated:
  - role: generated
    harness: claude-code:2.1.322
    model: anthropic:claude-sonnet-5-5
    effort: medium
    surface: cli
    session: sha-256-64:1e5db78070fe05b4
    at: "2026-11-23T08:23:35Z"
---

<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright Tony Burns -->

# Deploy the macOS exporters

Run these steps from the `agent-capacity` checkout on the exporter host using the ops account. Use an ops-owned checkout and binary because the installer runs as root. The installer reads the collector endpoint and host label from the live main serve plist.

1. Fetch `main`, check out the merged commit, and build its binary. Replace `MERGED_SHA` with the full commit SHA given by the merge.

   ```bash
   git fetch origin main
   git switch --detach MERGED_SHA
   mise install bazelisk
   bazelisk build --config=release //cmd/agent-capacity
   relative=$(bazelisk cquery --config=release --output=files //cmd/agent-capacity)
   exec_root=$(bazelisk info --config=release execution_root)
   binary="$exec_root/$relative"
   ```

2. Preview the binary replacement, plist installs, spool setup, and daemon restarts. The version string can match across builds, so the installer also compares the binary files.

   ```bash
   sudo ./deploy/macos/install.sh check --binary "$binary"
   ```

3. Apply the plan. Save the backup path printed by the installer.

   ```bash
   sudo ./deploy/macos/install.sh apply --binary "$binary"
   ```

4. In Mimir, query `agent_capacity_window_tokens{agent_account=~"codex-primary|claude-primary|claude-secondary"}` after an export interval. Check the account labels for fresh samples. Claude token metrics also need fresh window events in transcript files the exporter can find. The current reader scans immediate files in each configured directory, so a `config/projects` tree that stores transcripts only under project subdirectories needs a reader change before those accounts can produce metrics.

## Undo

Run rollback before retrying a failed apply. It uses the latest complete backup under `/var/backups`, restores the binary and the previous serve plists, and unloads and removes plists that the apply added. The Claude spool directories are retained.

```bash
sudo ./deploy/macos/install.sh rollback
```
