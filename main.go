package main

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"syscall"
)

func main() {
	log := newLogger(os.Stderr, underJournald(os.Stderr))
	if err := execute(os.Args[1:], os.Stdout, os.Stderr, log); err != nil {
		log.Error("Bridge stopped", "error", err)
		os.Exit(1)
	}
}

func execute(args []string, stdout, stderr io.Writer, log *slog.Logger) error {
	defaults, err := defaultPaths()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("superwhisper-sonos", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaults.config, "Bridge TOML configuration")
	statePath := flags.String("state", defaults.state, "Recovery journal")
	socketPath := flags.String("socket", defaults.socket, "Superwhisper control socket")
	sonosPath := flags.String("sonos", "sonos", "Path to the sonoscli executable")
	check := flags.Bool("check", false, "Read settings and speaker status without changing playback")
	restore := flags.Bool("restore", false, "Restore pending changes and exit")
	showVersion := flags.Bool("version", false, "Print version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *showVersion {
		_, err := fmt.Fprintln(stdout, "superwhisper-sonos", buildVersion())
		return err
	}
	if *check && *restore {
		return errors.New("--check and --restore cannot be combined")
	}
	binary, err := exec.LookPath(*sonosPath)
	if err != nil {
		return fmt.Errorf("find sonoscli (install sonos or use --sonos): %w", err)
	}
	sonos := &sonosCLI{binary: binary}
	whisper := &whisperSocket{path: *socketPath}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *check {
		cfg, err := loadConfig(*configPath)
		if err != nil {
			return err
		}
		return checkStatus(ctx, sonos, whisper, cfg, stdout)
	}
	lock, err := lockState(*statePath)
	if err != nil {
		return err
	}
	defer lock.Close()
	b, err := newBridge(sonos, *statePath, log)
	if err != nil {
		return err
	}
	if *restore {
		_, err := b.restore(ctx)
		return err
	}
	configLoader := func() (config, error) { return loadConfig(*configPath) }
	cfg, err := configLoader()
	if err != nil {
		return err
	}
	log.Info("Following Superwhisper", "devices", cfg.Devices, "volume_factor", cfg.VolumeFactor)
	return runBridge(ctx, b, newMonitor(whisper, log), configLoader)
}

// buildVersion reports the version the Go toolchain stamped into the binary:
// the module version from a git tag, or a pseudo-version from the commit.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}
	return info.Main.Version
}

// newLogger writes text logs. journald timestamps every line itself, so the
// logger's own time attribute is dropped there.
func newLogger(w io.Writer, journald bool) *slog.Logger {
	var opts slog.HandlerOptions
	if journald {
		opts.ReplaceAttr = func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}
	}
	return slog.New(slog.NewTextHandler(w, &opts))
}

// underJournald reports whether f is the stream systemd connected to the
// journal. JOURNAL_STREAM names that stream's device and inode; comparing them
// avoids trusting a variable inherited by a process whose stderr was redirected.
func underJournald(f *os.File) bool {
	stream := os.Getenv("JOURNAL_STREAM")
	if stream == "" {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && stream == fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}

func checkStatus(ctx context.Context, sonos sonosClient, whisper whisperClient, cfg config, out io.Writer) error {
	behavior, err := whisper.Behavior(ctx)
	if err != nil {
		return err
	}
	type speakerSummary struct {
		Target string         `json:"target"`
		Device device         `json:"device,omitzero"`
		State  transportState `json:"state,omitzero"`
		Volume *int           `json:"volume,omitzero"`
		Error  string         `json:"error,omitzero"`
	}
	reply := struct {
		Behavior     string           `json:"behavior"`
		Devices      []string         `json:"devices"`
		VolumeFactor float64          `json:"volume_factor"`
		Speakers     []speakerSummary `json:"speakers"`
	}{Behavior: behavior, Devices: cfg.Devices, VolumeFactor: cfg.VolumeFactor}
	// An unreachable speaker is reported in place so it does not hide the others.
	failed := 0
	for _, target := range cfg.Devices {
		summary := speakerSummary{Target: target}
		if status, err := sonos.Status(ctx, target); err != nil {
			failed++
			summary.Error = err.Error()
		} else {
			summary.Device, summary.State, summary.Volume = status.Device, status.Transport.State, status.Volume
		}
		reply.Speakers = append(reply.Speakers, summary)
	}
	if err := json.MarshalEncode(jsontext.NewEncoder(out, jsontext.WithIndent("  ")), reply); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("could not read %d of %d speakers", failed, len(cfg.Devices))
	}
	return nil
}
