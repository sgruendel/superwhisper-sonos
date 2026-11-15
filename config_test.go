package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConfigDefaultsAndSinuousPrecedence(t *testing.T) {
	dir := t.TempDir()
	configPath, sinuousPath := filepath.Join(dir, "bridge.toml"), filepath.Join(dir, "sinuous.toml")
	writeTestFile(t, configPath, "sinuous_config = '"+sinuousPath+"'\ndevices = ['Fallback']\n")
	for _, sinuous := range []string{"", "[ui]\nnerd_font = true\n", "[core]\ndevices = []\n"} {
		writeTestFile(t, sinuousPath, sinuous)
		cfg, err := loadConfig(configPath)
		if err != nil || !reflect.DeepEqual(cfg.Devices, []string{"Fallback"}) || cfg.VolumeFactor != 0.25 {
			t.Fatal(cfg, err)
		}
	}
	writeTestFile(t, sinuousPath, "[core]\ndevices = ['Büro', '192.0.2.2']\n[ui]\nnerd_font = true\n")
	cfg, err := loadConfig(configPath)
	if err != nil || !reflect.DeepEqual(cfg.Devices, []string{"Büro", "192.0.2.2"}) {
		t.Fatal(cfg, err)
	}
}

func TestInvalidConfig(t *testing.T) {
	for _, data := range []string{
		"volume_factor = 0", "volume_factor = 1", "volume_factor = -0.5", "volume_factor = nan",
		"volume_factor = inf", "volume_factor = true", "volume_factor = 'bad'",
		"devices = [1]", "devices = ['  ']", "devices = []", "[broken",
	} {
		t.Run(data, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeTestFile(t, path, "sinuous_config = ''\n"+data+"\n")
			if _, err := loadConfig(path); err == nil {
				t.Fatal("accepted invalid config", data)
			}
		})
	}
}

func TestMalformedSinuousConfigDoesNotSelectFallback(t *testing.T) {
	dir := t.TempDir()
	configPath, sinuousPath := filepath.Join(dir, "bridge.toml"), filepath.Join(dir, "sinuous.toml")
	writeTestFile(t, sinuousPath, "[core]\ndevices = 'invalid'\n")
	writeTestFile(t, configPath, "devices = ['Fallback']\nsinuous_config = '"+sinuousPath+"'\n")
	if _, err := loadConfig(configPath); err == nil {
		t.Fatal("expected malformed Sinuous config error")
	}
}

func TestXDGPathsAndHomeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "runtime"))
	t.Setenv("SUPERWHISPER_RUNTIME", "")
	paths, err := defaultPaths()
	if err != nil || paths.config != filepath.Join(home, "config/superwhisper-sonos/config.toml") ||
		paths.state != filepath.Join(home, "state/superwhisper-sonos/recovery.json") ||
		paths.socket != filepath.Join(home, "runtime/superwhisper/control.sock") {
		t.Fatal(paths, err)
	}
	for input, want := range map[string]string{"~": home, "~/config": filepath.Join(home, "config"), "relative": "relative"} {
		if got, err := expandHome(input); err != nil || got != want {
			t.Fatal(input, got, err)
		}
	}
	t.Setenv("SUPERWHISPER_RUNTIME", filepath.Join(home, "custom"))
	paths, err = defaultPaths()
	if err != nil || paths.socket != filepath.Join(home, "custom/control.sock") {
		t.Fatal(paths, err)
	}
}
