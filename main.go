package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
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
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *showVersion {
		_, err := fmt.Fprintln(stdout, "superwhisper-sonos", version)
		return err
	}
	if *check && *restore {
		return fmt.Errorf("--check and --restore cannot be combined")
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

func checkStatus(ctx context.Context, sonos sonosClient, whisper whisperClient, cfg config, out io.Writer) error {
	behavior, err := whisper.Behavior(ctx)
	if err != nil {
		return err
	}
	type speakerSummary struct {
		Device device `json:"device"`
		State  string `json:"state"`
		Volume *int   `json:"volume"`
	}
	reply := struct {
		Behavior     string           `json:"behavior"`
		Devices      []string         `json:"devices"`
		VolumeFactor float64          `json:"volume_factor"`
		Speakers     []speakerSummary `json:"speakers"`
	}{Behavior: behavior, Devices: cfg.Devices, VolumeFactor: cfg.VolumeFactor}
	for _, target := range cfg.Devices {
		status, err := sonos.Status(ctx, target)
		if err != nil {
			return err
		}
		reply.Speakers = append(reply.Speakers, speakerSummary{
			Device: status.Device, State: status.Transport.State, Volume: status.Volume,
		})
	}
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(reply)
}
