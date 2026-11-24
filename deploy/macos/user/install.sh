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

PATH=/usr/bin:/bin:/usr/sbin:/sbin
repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
label=dev.tbhb.agent-capacity-serve
live=$HOME/Library/LaunchAgents/$label.plist
backup=$HOME/.local/state/tbhb/agent-capacity-tony/$label.plist.bak
domain=gui/$(id -u)

# Keep the private collector address on the host: copy every OTEL_ key from the live file.
render() {
  local xml keys key value
  xml=$(sed "s|@HOME@|$HOME|g" "$repo_dir/$label.plist" | plutil -convert xml1 -o - -) || return 1
  [[ -f $live ]] || {
    printf '%s\n' "$xml"
    return 0
  }
  keys=$(/usr/libexec/PlistBuddy -c 'Print :EnvironmentVariables' "$live" | sed -n 's/^    \([^ ][^ ]*\) = .*/\1/p') # cspell:disable-line
  for key in $keys; do
    [[ $key == OTEL_* ]] || continue
    value=$(plutil -extract "EnvironmentVariables.$key" raw -expect string -o - "$live") || return 1
    xml=$(printf '%s' "$xml" | plutil -insert "EnvironmentVariables.$key" -string "$value" -o - -) || return 1
  done
  printf '%s\n' "$xml"
}

restart() {
  launchctl bootout "$domain/$label" 2>/dev/null || true
  for _ in 1 2 3 4 5; do
    launchctl bootstrap "$domain" "$live" && return 0
    sleep 1
  done
  fail "Bootstrap failed for $label."
}

if [[ $action == rollback ]]; then
  [[ -f $backup ]] || fail "No backup at $backup."
  install -m 644 "$backup" "$live"
  restart
  exit 0
fi

new=$(render) || fail 'Cannot render the plist.'
if [[ -f $live ]]; then
  current=$(plutil -convert xml1 -o - "$live") || fail 'Cannot read the live plist.'
  if [[ $new == "$current" ]]; then
    printf 'Live plist already matches the repository.\n'
    exit 0
  fi
fi
printf 'Install %s\n' "$live"
[[ $action == apply ]] || exit 0
if [[ -f $live ]]; then
  cp -p "$live" "$backup"
fi
printf '%s\n' "$new" >"$live.new"
plutil -lint "$live.new" >/dev/null
mv "$live.new" "$live"
restart
