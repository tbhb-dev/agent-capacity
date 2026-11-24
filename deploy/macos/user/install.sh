#!/bin/bash
# SPDX-License-Identifier: Apache-2.0
# Copyright Tony Burns

set -euo pipefail

fail() {
  printf '%s\n' "$*" >&2
  exit 1
}

action=${1:-}
case "$action" in
check | apply | rollback) ;;
*) fail 'Usage: install.sh check | apply | rollback' ;;
esac
((EUID != 0)) || fail 'Run as the user that owns the service, not root.'
# The path goes into sed and XML text, so accept only plain characters.
[[ $HOME =~ ^/[A-Za-z0-9._/-]+$ ]] || fail 'HOME has characters this installer does not support.'

PATH=/usr/bin:/bin:/usr/sbin:/sbin
umask 077
repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
label=dev.tbhb.agent-capacity-serve
live_dir=$HOME/Library/LaunchAgents
live=$live_dir/$label.plist
backup_dir=$HOME/.local/state/tbhb/agent-capacity-tony/$label.bak
domain=gui/$(id -u)
pb=/usr/libexec/PlistBuddy # cspell:disable-line

loaded() {
  launchctl print "$domain/$label" >/dev/null 2>&1
}

# Keep the private collector address on the host: copy every OTEL_ key from the live file.
render() {
  local xml keys key value
  xml=$(sed "s|@HOME@|$HOME|g" "$repo_dir/$label.plist" | plutil -convert xml1 -o - -) || return 1
  if [[ -f $live ]]; then
    # A live file with no environment dictionary has nothing to copy.
    keys=$("$pb" -c 'Print :EnvironmentVariables' "$live" 2>/dev/null | sed -n 's/^    \([^ ][^ ]*\) = .*/\1/p') || keys=
    for key in $keys; do
      [[ $key == OTEL_* ]] || continue
      [[ $key != *HEADERS* ]] || fail "Refusing to copy $key: it can hold a credential."
      value=$(plutil -extract "EnvironmentVariables.$key" raw -expect string -o - "$live") || return 1
      xml=$(printf '%s' "$xml" | plutil -insert "EnvironmentVariables.$key" -string "$value" -o - -) || return 1
    done
  fi
  printf '%s\n' "$xml"
}

# Boot the job out and back in, retrying while launchd finishes removing the old copy.
restart() {
  launchctl bootout "$domain/$label" 2>/dev/null || true
  for _ in 1 2 3 4 5; do
    launchctl bootstrap "$domain" "$live" && return 0
    sleep 1
  done
  return 1
}

# Put a plist in place through a temp file in the same directory, so a failure leaves the old file.
place() {
  local tmp
  tmp=$(mktemp "$live.XXXXXX") || return 1
  cp -p "$1" "$tmp" && plutil -lint "$tmp" >/dev/null && mv "$tmp" "$live" && return 0
  rm -f "$tmp"
  return 1
}

if [[ $action == rollback ]]; then
  [[ -f $backup_dir/applied ]] || fail "Nothing to roll back in $backup_dir."
  if [[ -f $backup_dir/plist ]]; then
    place "$backup_dir/plist" || fail 'Cannot restore the saved plist.'
    if [[ -f $backup_dir/was-loaded ]]; then
      restart || fail "Bootstrap failed for $label."
    else
      launchctl bootout "$domain/$label" 2>/dev/null || true
    fi
  else
    launchctl bootout "$domain/$label" 2>/dev/null || true
    rm -f "$live"
  fi
  rm -rf "$backup_dir"
  printf 'Rolled back %s\n' "$label"
  exit 0
fi

new=$(render) || fail 'Cannot render the plist.'
if [[ -f $live ]]; then
  current=$(plutil -convert xml1 -o - "$live") || fail 'Cannot read the live plist.'
  if [[ $new == "$current" ]] && loaded; then
    printf 'Live plist already matches the repository.\n'
    exit 0
  fi
fi
printf 'Install %s\n' "$live"
[[ $action == apply ]] || exit 0

mkdir -p "$live_dir" "$backup_dir.tmp"
staged=$(mktemp "$backup_dir.tmp/new.XXXXXX")
printf '%s\n' "$new" >"$staged"
plutil -lint "$staged" >/dev/null || fail 'The rendered plist is invalid.'

# Save the rollback point only after the replacement is staged and valid.
rm -rf "$backup_dir"
mkdir "$backup_dir"
if [[ -f $live ]]; then
  cp -p "$live" "$backup_dir/plist"
fi
if loaded; then
  : >"$backup_dir/was-loaded"
fi
: >"$backup_dir/applied"

place "$staged" || fail 'Cannot place the new plist.'
rm -rf "$backup_dir.tmp"
if ! restart; then
  printf 'Bootstrap failed for %s; restoring the previous state.\n' "$label" >&2
  "$0" rollback || true
  fail 'Apply failed and was rolled back.'
fi
printf 'Applied %s\n' "$label"
