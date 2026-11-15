#!/usr/bin/env bash
set -euo pipefail

config_dir="${XDG_CONFIG_HOME:-$HOME/.config}"
state_dir="${XDG_STATE_HOME:-$HOME/.local/state}/superwhisper-sonos"
unit="$config_dir/systemd/user/superwhisper-sonos.service"
binary="$HOME/.local/bin/superwhisper-sonos"

# Stopping the service restores any Sonos changes it still holds.
if [[ -e "$unit" ]]; then
  systemctl --user disable --now superwhisper-sonos.service
fi

# A speaker that was unreachable during shutdown stays in the journal.
# Retry once; keep the binary if that fails so the user can retry later.
journal="$state_dir/recovery.json"
if [[ -s "$journal" && "$(<"$journal")" != "[]" && -x "$binary" ]]; then
  if ! "$binary" --restore; then
    rm -f "$unit"
    systemctl --user daemon-reload
    echo "Some Sonos changes are still pending. Run $binary --restore when the" >&2
    echo 'speakers are reachable, then run make uninstall again.' >&2
    exit 1
  fi
fi

rm -f "$unit" "$binary"
systemctl --user daemon-reload

echo "Uninstalled. Kept config in $config_dir/superwhisper-sonos and state in $state_dir."
