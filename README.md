# superwhisper-sonos

Linux background service that follows Superwhisper recording sessions and controls Sonos groups using sonoscli

| Superwhisper playback setting | Sonos while recording | After recording or cancellation |
| --- | --- | --- |
| Pause | Pause groups currently playing | Resume groups paused by the bridge |
| Muffle / Duck (Lower) | Reduce group volume to 25% of its previous value | Restore previous volume |
| Mute | Mute groups currently playing | Unmute groups muted by the bridge |
| Keep Playing | Leave playback alone | Leave playback alone |

The bridge uses Superwhisper's local control socket and
[steipete/sonoscli](https://github.com/steipete/sonoscli) for local network control.
No Sonos cloud authorization is required. Muffle uses volume reduction on Sonos;
it does not apply Superwhisper's local audio filter.

## Requirements

- Linux and Go 1.27 or later to build.
- Superwhisper for Linux, running with its local control socket.
- The `sonos` executable from sonoscli, on `PATH` or selected with `--sonos`.
- Network access to the selected Sonos speakers on TCP port 1400.
- systemd for the supplied user service; the binary can also run directly.

The bridge compiles to one binary. Its only Go dependency is a TOML parser;
sonoscli remains a separate executable. staticcheck and govulncheck are pinned
in `go.mod` as development tools and are not compiled into the bridge.

## Build and run

Install sonoscli if it is not already installed:

```sh
go install github.com/steipete/sonoscli/cmd/sonos@latest
```

Ensure the installed `sonos` executable is on your `PATH`, then build:

```sh
make build
mkdir -p ~/.config/superwhisper-sonos
cp config.example.toml ~/.config/superwhisper-sonos/config.toml
```

Configure devices as described below. Check the settings and speaker status
without changing playback:

```sh
./bin/superwhisper-sonos --check
```

An unreachable speaker is listed with its error, and the command exits nonzero.

For a foreground session:

```sh
./bin/superwhisper-sonos
```

Only one bridge may use a recovery journal at a time. Stop the installed
service before starting a foreground session. `--check` can run alongside it.

## Devices and volume

Edit `~/.config/superwhisper-sonos/config.toml`. Changes apply to the next recording.

```toml
sinuous_config = "~/.config/sinuous/config.toml"
devices = ["Kitchen", "192.0.2.10"]
volume_factor = 0.25
```

If Sinuous has a nonempty `[core].devices` list, that list takes precedence:

```toml
[core]
devices = ["Kitchen", "speaker.local"]
```

An absent Sinuous config or an empty device list uses the bridge's `devices`
instead. Set `sinuous_config = ""` to use only the bridge's devices.
Invalid TOML produces an error instead of silently choosing other speakers.

Targets may be IP addresses, DNS hostnames, or Sonos room names. Hostnames are
resolved to IPv4 first; other names are passed to sonoscli's room lookup.
IP addresses avoid discovery on each recording. Make sure they remain stable
or update them when DHCP changes them.

`volume_factor` must be greater than zero and less than one. The default, 0.25,
turns Sonos volume 20 into 5. Reduction rounds down, with a minimum of 1 for
nonzero volumes. It never raises the volume. This is a fraction of Sonos's
volume scale, not a precise acoustic loudness ratio.

Commands affect the selected room's entire current Sonos group. Multiple
targets in the same group are adjusted once. Sonos group volume preserves
relative room levels, subject to rounding.

## Install the user service

```sh
make install
systemctl --user status superwhisper-sonos
journalctl --user -u superwhisper-sonos -n 30
```

The installer puts the bridge in `~/.local/bin`, preserves an existing config,
and enables and restarts `superwhisper-sonos.service`. Its service `PATH`
includes `~/.local/bin`, `~/go/bin`, and the standard system binary directories.

To disable the integration and restore outstanding changes:

```sh
systemctl --user disable --now superwhisper-sonos
```

To remove the service and binary, restoring outstanding changes first:

```sh
make uninstall
```

It keeps your config and the state directory. If a speaker is unreachable,
it keeps the binary and asks you to run `--restore` later.

To restore a pending journal manually after stopping the service:

```sh
superwhisper-sonos --restore
```

## Recording and recovery

The bridge checks Superwhisper every 200 ms. It reads
`audio_get.config.defaultPlaybackBehavior` once per recording, and restores
when the phase leaves `recording`, including during transcription. A separate
goroutine keeps watching recording state while Sonos requests are in flight,
so releasing the key cancels adjustments that have not yet been sent.

Stopped, already paused, and already muted groups are left alone. A paused
group is resumed only if it is still paused on the same source and coordinator.
A reduced volume is restored only if it still matches the bridge's value, and a
muted group is unmuted only if it is still muted, so manual changes win. Manual
changes that produce exactly the bridge's volume, paused, or muted state cannot
be distinguished.

Superwhisper counts as lost after three consecutive failed checks, so a single
slow reply does not resume and re-pause Sonos mid-recording.

Every change is written to a private, atomic recovery journal before sending
the command. This also covers a request whose response is lost after the
speaker applies it. Changes are restored on recording end, cancellation,
loss of Superwhisper, shutdown, and service restart. An unreachable speaker
remains journaled and is retried every five seconds; new sessions wait for
pending restoration to complete.

If the coordinator changes, restoration is skipped. Avoid regrouping rooms
during a recording: membership changes behind an unchanged coordinator cannot
be detected by this integration. Group volume restoration may round individual
room levels.

## Options and paths

```text
--config PATH   Bridge TOML config
--state PATH    Recovery journal
--socket PATH   Superwhisper control socket
--sonos PATH    sonoscli executable (default: sonos on PATH)
--check         Read settings and status without changing playback
--restore       Restore pending changes and exit
--version       Print version and exit
```

Defaults follow `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, and `XDG_RUNTIME_DIR`:

| File | Default |
| --- | --- |
| Bridge config | `~/.config/superwhisper-sonos/config.toml` |
| Sinuous config | `~/.config/sinuous/config.toml` |
| Recovery journal | `~/.local/state/superwhisper-sonos/recovery.json` |
| Superwhisper socket | `/run/user/<uid>/superwhisper/control.sock` |

`SUPERWHISPER_RUNTIME` overrides the directory containing `control.sock`.
Logs and `--check` output omit dictation text and Sonos source URLs.

## Development

```sh
make test      # Tests and Go race detector; requires a C compiler
make generate  # Regenerate enum String methods after changing their constants
make check     # go vet, gofmt, go mod tidy, and staticcheck
make vuln      # govulncheck; needs network access
make build     # Static binary with CGO disabled
```

Tests use temporary Unix sockets and a fake sonoscli executable. They cover
playback and volume restoration, cancellation during slow requests, manual
changes, disconnects, shutdown, uncertain writes, journal recovery,
configuration precedence, process timeouts, and concurrent instance exclusion.
They do not change actual speaker playback.
