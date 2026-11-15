#!/usr/bin/env bash
set -euo pipefail

source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
config_dir="${XDG_CONFIG_HOME:-$HOME/.config}"

if [[ ! -x "$source_dir/bin/superwhisper-sonos" ]]; then
  echo 'Build the bridge first with make build.' >&2
  exit 1
fi
if ! command -v sonos >/dev/null 2>&1; then
  echo 'Install sonoscli first; the sonos executable must be on PATH.' >&2
  exit 1
fi

# Existing installations keep their config and recovery journal.
install -Dm755 "$source_dir/bin/superwhisper-sonos" "$HOME/.local/bin/superwhisper-sonos"
if [[ ! -e "$config_dir/superwhisper-sonos/config.toml" ]]; then
  install -Dm600 "$source_dir/config.example.toml" "$config_dir/superwhisper-sonos/config.toml"
fi
install -Dm644 "$source_dir/packaging/superwhisper-sonos.service" \
  "$config_dir/systemd/user/superwhisper-sonos.service"
systemctl --user daemon-reload
systemctl --user enable superwhisper-sonos.service
systemctl --user restart superwhisper-sonos.service
