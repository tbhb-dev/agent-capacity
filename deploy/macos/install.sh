#!/bin/bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Tony Burns

set -euo pipefail

fail() {
  printf '%s\n' "$*" >&2
  exit 1
}

usage() {
  fail 'Usage: install.sh check [--binary FILE] | apply --binary FILE | rollback'
}

action=${1:-}
case "$action" in
check | apply | rollback) shift ;;
*) usage ;;
esac
candidate=
if (($# > 0)); then
  [[ $action != rollback && $# == 2 && $1 == --binary && -n $2 ]] || usage
  candidate=$2
fi
[[ $action != apply || -n $candidate ]] || usage
((EUID == 0)) || fail 'Run as root.'

PATH=/usr/bin:/bin:/usr/sbin:/sbin
umask 077
repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
live_dir=/Library/LaunchDaemons
binary=/usr/local/bin/agent-capacity
backup_root=/var/backups
serve=dev.tbhb.agent-capacity.serve
labels=("$serve" "$serve.claude-claude-primary" "$serve.claude-claude-secondary")
spool_dirs=(/Users/capacity/spool-claude-{primary,secondary})

running() {
  launchctl print "system/$1" 2>/dev/null |
    grep -E '^[[:space:]]*state = running$' >/dev/null
}

unload() {
  local service="system/$1"
  if launchctl print "$service" >/dev/null 2>&1; then
    launchctl bootout "$service"
  fi
}

reload() {
  local label=$1 attempt
  unload "$label"

  for attempt in 1 2 3 4 5; do
    if launchctl bootstrap system "$live_dir/$label.plist"; then
      break
    fi
    ((attempt < 5)) || fail "Bootstrap failed for $label."
    sleep 1
  done

  for attempt in 1 2 3 4 5; do
    if running "$label"; then
      printf 'Running %s\n' "$label"
      return
    fi
    ((attempt < 5)) || fail "$label did not reach the running state."
    sleep 1
  done
}

restore() {
  local backup=$1 label saved live
  printf 'Restoring %s\n' "$backup"
  if ! cmp -s "$backup/agent-capacity" "$binary"; then
    install -o root -g wheel -m 755 "$backup/agent-capacity" "$binary"
  fi
  for label in "${labels[@]}"; do
    saved=$backup/$label.plist
    live=$live_dir/$label.plist
    if [[ ! -f $saved ]]; then
      unload "$label"
      rm -f "$live"
    else
      if ! cmp -s "$saved" "$live"; then
        install -o root -g wheel -m 644 "$saved" "$live"
      fi
      reload "$label"
    fi
  done
}

if [[ $action == rollback ]]; then
  backup=
  for directory in "$backup_root"/agent-capacity-*; do
    if [[ -d $directory ]]; then
      backup=$directory
    fi
  done
  [[ -n $backup ]] || fail 'No backup found.'
  [[ -f $backup/complete ]] || fail "The newest backup $backup is incomplete, so the last apply changed nothing."
  restore "$backup"
  exit 0
fi

# Keep the private collector address and host label on the host. Reuse the main serve's values.
endpoint=$(plutil -extract EnvironmentVariables.OTEL_EXPORTER_OTLP_ENDPOINT raw -expect string "$live_dir/$serve.plist" 2>/dev/null) || fail 'Cannot read the live collector endpoint.'
[[ -n $endpoint ]] || fail 'The live collector endpoint is empty.'
resource=$(plutil -extract EnvironmentVariables.OTEL_RESOURCE_ATTRIBUTES raw -expect string "$live_dir/$serve.plist" 2>/dev/null) || fail 'Cannot read the live resource attributes.'
[[ -n $resource ]] || fail 'The live resource attributes are empty.'

pb=/usr/libexec/PlistBuddy # cspell:disable-line
live_env=$("$pb" -c 'Print :EnvironmentVariables' "$live_dir/$serve.plist") || fail 'Cannot read the live environment.'
key_lines=$(sed -n 's/^    \([^ ][^ ]*\) = .*/\1/p' <<<"$live_env")
live_keys=()
while IFS= read -r key; do
  [[ $key =~ ^[A-Za-z0-9_]+$ ]] || fail "Unsupported live environment key: $key"
  live_keys+=("$key")
done <<<"$key_lines"

# Start from the repository file and add each live environment key it does not define.
render() {
  local xml key value
  xml=$(plutil -convert xml1 -o - "$repo_dir/$1.plist") || return 1
  for key in "${live_keys[@]}"; do
    if ! plutil -extract "EnvironmentVariables.$key" raw -o - "$repo_dir/$1.plist" >/dev/null 2>&1; then
      value=$(plutil -extract "EnvironmentVariables.$key" raw -expect string -o - "$live_dir/$serve.plist") || return 1
      xml=$(printf '%s' "$xml" | plutil -insert "EnvironmentVariables.$key" -string "$value" -o - -) || return 1
    fi
  done
  printf '%s\n' "$xml"
}

version=$("$binary" version)
printf 'Installed binary: %s\n' "$version"
binary_changed=0
if [[ -n $candidate ]]; then
  [[ -f $candidate && -x $candidate ]] || fail 'The candidate binary must be an executable file.'
  if [[ $candidate != /* ]]; then
    candidate=$PWD/$candidate
  fi
  version=$("$candidate" version)
  printf 'Candidate binary: %s\n' "$version"
  if ! cmp -s "$candidate" "$binary"; then
    binary_changed=1
    printf 'Replace %s\n' "$binary"
  fi
elif [[ $action == check ]]; then
  printf 'Pass --binary FILE to compare the replacement and plan its daemon restarts.\n'
fi

changed_plists=()
restart_labels=()
for label in "${labels[@]}"; do
  live=$live_dir/$label.plist
  repo_hash=$(render "$label" | shasum -a 256)
  live_hash=missing
  if [[ -f $live ]]; then
    live_hash=$(plutil -convert xml1 -o - "$live" | shasum -a 256)
  fi
  if [[ $repo_hash != "$live_hash" ]]; then
    changed_plists+=("$label")
    restart_labels+=("$label")
    printf 'Install %s\n' "$live"
  elif ((binary_changed)) || ! running "$label"; then
    restart_labels+=("$label")
  fi
done

spools_changed=0
for spool in "${spool_dirs[@]}"; do
  if [[ ! -d $spool ]] || ! metadata=$(stat -f '%Su:%Sg %Lp' "$spool") ||
    [[ $metadata != 'capacity:capacity 700' ]]; then
    spools_changed=1
    printf 'Set up %s (capacity:capacity, mode 700)\n' "$spool"
  fi
done
for label in "${restart_labels[@]+"${restart_labels[@]}"}"; do
  printf 'Reload %s\n' "$label"
done

[[ $action == apply ]] || exit 0
if ((! binary_changed && ! spools_changed)) && [[ -z ${restart_labels[0]:-} ]]; then
  printf 'No changes to apply.\n'
  exit 0
fi
backup=$backup_root/agent-capacity-$(date -u +%Y%m%dT%H%M%SZ)
mkdir "$backup"
cp -p "$binary" "$backup/agent-capacity"
for label in "${labels[@]}"; do
  live=$live_dir/$label.plist
  if [[ -f $live ]]; then
    cp -p "$live" "$backup/"
  fi
done
touch "$backup/complete"
printf 'Saved backup to %s\n' "$backup"

rollback_on_failure() {
  local status=$?
  trap - EXIT
  if ((status != 0)); then
    printf 'Apply failed. Restoring %s\n' "$backup" >&2
    (restore "$backup") || printf 'The automatic restore failed. Run rollback.\n' >&2
  fi
  exit "$status"
}
trap rollback_on_failure EXIT

if ((binary_changed)); then
  install -o root -g wheel -m 755 "$candidate" "$binary"
fi
if ((spools_changed)); then
  sudo -u capacity -g capacity /usr/bin/install -d -m 700 "${spool_dirs[@]}"
fi
for label in "${changed_plists[@]+"${changed_plists[@]}"}"; do
  render "$label" >"$backup/$label.new"
  install -o root -g wheel -m 644 "$backup/$label.new" "$live_dir/$label.plist"
  rm "$backup/$label.new"
done
for label in "${restart_labels[@]+"${restart_labels[@]}"}"; do
  reload "$label"
done
