package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type config struct {
	Devices       []string `toml:"devices"`
	SinuousConfig string   `toml:"sinuous_config"`
	VolumeFactor  float64  `toml:"volume_factor"`
}

type paths struct {
	config, state, socket string
}

func defaultPaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, err
	}
	configHome := envOr("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	stateHome := envOr("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	runtime := envOr("SUPERWHISPER_RUNTIME", filepath.Join(
		envOr("XDG_RUNTIME_DIR", fmt.Sprintf("/run/user/%d", os.Getuid())), "superwhisper"))
	return paths{
		config: filepath.Join(configHome, "superwhisper-sonos", "config.toml"),
		state:  filepath.Join(stateHome, "superwhisper-sonos", "recovery.json"),
		socket: filepath.Join(runtime, "control.sock"),
	}, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

func loadConfig(path string) (config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return config{}, err
	}
	cfg := config{
		VolumeFactor: 0.25,
		SinuousConfig: filepath.Join(envOr("XDG_CONFIG_HOME", filepath.Join(home, ".config")),
			"sinuous", "config.toml"),
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return config{}, fmt.Errorf("read bridge config: %w", err)
	}
	if math.IsNaN(cfg.VolumeFactor) || cfg.VolumeFactor <= 0 || cfg.VolumeFactor >= 1 {
		return config{}, fmt.Errorf("volume_factor must be greater than 0 and less than 1")
	}
	if cfg.SinuousConfig != "" {
		cfg.SinuousConfig, err = expandHome(cfg.SinuousConfig)
		if err != nil {
			return config{}, err
		}
		var sinuous struct {
			Core struct {
				Devices []string `toml:"devices"`
			} `toml:"core"`
		}
		if _, err := toml.DecodeFile(cfg.SinuousConfig, &sinuous); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return config{}, fmt.Errorf("read Sinuous config: %w", err)
			}
		} else if len(sinuous.Core.Devices) > 0 {
			cfg.Devices = sinuous.Core.Devices
		}
	}
	if len(cfg.Devices) == 0 {
		return config{}, fmt.Errorf("configure core.devices in Sinuous or devices in the bridge config")
	}
	for _, device := range cfg.Devices {
		if strings.TrimSpace(device) == "" {
			return config{}, fmt.Errorf("device names or addresses must not be empty")
		}
	}
	return cfg, nil
}
