package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testWhisperSocket(t *testing.T, reply func(command string) string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				var request struct {
					Version int    `json:"version"`
					Command string `json:"command"`
				}
				if err := json.UnmarshalDecode(jsontext.NewDecoder(conn), &request); err != nil {
					t.Error(err)
					return
				}
				if request.Version != 1 {
					t.Error("wrong protocol version", request.Version)
				}
				response := reply(request.Command)
				if response == "wait" {
					_, _ = io.Copy(io.Discard, conn)
					return
				}
				_, _ = io.WriteString(conn, response)
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	return path
}

func TestWhisperSocketProtocol(t *testing.T) {
	path := testWhisperSocket(t, func(command string) string {
		switch command {
		case "status":
			return "{\"phase\":\"recording\",\"session_id\":7,\"last\":{\"text\":\"private dictation\"}}\n"
		case "audio_get":
			return "{\"config\":{\"defaultPlaybackBehavior\":\"muffle\"}}\n"
		default:
			t.Error("unexpected command", command)
			return "{}\n"
		}
	})
	client := &whisperSocket{path: path}
	status, err := client.Status(context.Background())
	if err != nil || status.Phase != "recording" || status.SessionID == nil || *status.SessionID != 7 {
		t.Fatal(status, err)
	}
	behavior, err := client.Behavior(context.Background())
	if err != nil || behavior != "muffle" {
		t.Fatal(behavior, err)
	}
}

func TestWhisperRejectsMalformedReplies(t *testing.T) {
	for _, response := range []string{
		"null\n", "{}\n", "not JSON\n", "{\"error\":\"unavailable\"}\n",
		"{\"phase\":\"recording\"}\n", "{\"phase\":\"idle\"}",
		strings.Repeat(" ", 2*1024*1024) + "{}\n",
	} {
		path := testWhisperSocket(t, func(string) string { return response })
		if _, err := (&whisperSocket{path}).Status(context.Background()); err == nil {
			t.Fatal("accepted malformed or oversized reply")
		}
	}
}

func TestWhisperErrorsIncludeMessage(t *testing.T) {
	long := "x" + strings.Repeat("ü", 150) // the 200-byte limit splits a "ü"
	for response, want := range map[string]string{
		"{\"error\":\"Unknown command\"}\n": "status request rejected by Superwhisper: Unknown command",
		"{\"error\":{\"code\":42}}\n":       `status request rejected by Superwhisper: {"code":42}`,
		"{\"error\":\"" + long + "\"}\n":    "status request rejected by Superwhisper: x" + strings.Repeat("ü", 99) + "…",
	} {
		path := testWhisperSocket(t, func(string) string { return response })
		_, err := (&whisperSocket{path}).Status(context.Background())
		if err == nil || err.Error() != want {
			t.Errorf("got %v; want %q", err, want)
		}
	}
}

func TestWhisperSocketRespectsCancellation(t *testing.T) {
	path := testWhisperSocket(t, func(string) string { return "wait" })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := (&whisperSocket{path}).Status(ctx); err == nil {
		t.Fatal("expected cancellation")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("socket ignored cancellation")
	}
}

func fakeSonosCLI(t *testing.T) *sonosCLI {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sonos")
	if err := os.WriteFile(path, []byte(`#!/bin/sh
printf '%s\n' "$@" > "$SONOS_TEST_ARGS"
if [ "$SONOS_TEST_FAIL" = 1 ]; then
  echo 'speaker unavailable' >&2
  exit 1
fi
printf '%s\n' "$SONOS_TEST_REPLY"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SONOS_TEST_ARGS", filepath.Join(dir, "args"))
	t.Setenv("SONOS_TEST_REPLY", `{"volume":40}`)
	t.Setenv("SONOS_TEST_FAIL", "")
	return &sonosCLI{binary: path, lookupHost: func(context.Context, string) ([]string, error) {
		return nil, errors.New("not a hostname")
	}}
}

func TestSonosCLIArgumentsAndHostnameResolution(t *testing.T) {
	c := fakeSonosCLI(t)
	for _, target := range []string{"192.0.2.1", "Kitchen & Dining"} {
		if _, err := c.GroupVolume(context.Background(), target); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(os.Getenv("SONOS_TEST_ARGS"))
		if err != nil {
			t.Fatal(err)
		}
		flag := "--name"
		if target == "192.0.2.1" {
			flag = "--ip"
		}
		want := "--format\njson\n--timeout\n2s\n" + flag + "\n" + target + "\ngroup\nvolume\nget\n"
		if string(data) != want {
			t.Fatalf("arguments: %q, want %q", data, want)
		}
	}
	for mute, state := range map[bool]string{true: "on", false: "off"} {
		if err := c.SetGroupMute(context.Background(), "192.0.2.1", mute); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(os.Getenv("SONOS_TEST_ARGS"))
		if err != nil || !strings.HasSuffix(string(data), "\ngroup\nmute\n"+state+"\n") {
			t.Fatal(string(data), err)
		}
	}
	t.Setenv("SONOS_TEST_REPLY", `{"mute":true}`)
	if muted, err := c.GroupMute(context.Background(), "192.0.2.1"); err != nil || !muted {
		t.Fatal(muted, err)
	}
	t.Setenv("SONOS_TEST_REPLY", `{"volume":40}`)
	c.lookupHost = func(context.Context, string) ([]string, error) { return []string{"192.0.2.2"}, nil }
	if _, err := c.GroupVolume(context.Background(), "speaker.local"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(os.Getenv("SONOS_TEST_ARGS"))
	if err != nil || !strings.Contains(string(data), "--ip\n192.0.2.2\n") {
		t.Fatal(string(data), err)
	}
}

func TestSonosCLIValidatesVolumeAndTransport(t *testing.T) {
	c := fakeSonosCLI(t)
	for _, response := range []string{"{}", "null", `{"volume":-1}`, `{"volume":101}`,
		`{"volume":3.5}`, `{"volume":"40"}`, "not JSON"} {
		t.Setenv("SONOS_TEST_REPLY", response)
		if _, err := c.GroupVolume(context.Background(), "192.0.2.1"); err == nil {
			t.Fatal("accepted invalid volume", response)
		}
	}
	for _, response := range []string{"{}", `{"mute":null}`, `{"mute":"true"}`, `{"mute":1}`} {
		t.Setenv("SONOS_TEST_REPLY", response)
		if _, err := c.GroupMute(context.Background(), "192.0.2.1"); err == nil {
			t.Fatal("accepted invalid mute state", response)
		}
	}
	t.Setenv("SONOS_TEST_REPLY", `{"device":{"ip":"192.0.2.1","udn":"room-1"},"transport":{}}`)
	if _, err := c.Status(context.Background(), "192.0.2.1"); err == nil {
		t.Fatal("accepted missing transport state")
	}
	t.Setenv("SONOS_TEST_FAIL", "1")
	if err := c.Pause(context.Background(), "192.0.2.1"); err == nil || !strings.Contains(err.Error(), "speaker unavailable") {
		t.Fatal(err)
	}
}

func TestSonosCLICancelsHungProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sonos")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := (&sonosCLI{binary: path}).Pause(ctx, "192.0.2.1")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("subprocess did not time out", err)
	}
}

func TestCheckDoesNotWriteOrExposeSource(t *testing.T) {
	f := newFakeSonos()
	f.status.Position.TrackURI = "private-source"
	w := &fakeWhisper{behavior: "muffle"}
	var output bytes.Buffer
	err := checkStatus(context.Background(), f, w, config{Devices: []string{"Office"}, VolumeFactor: 0.25}, &output)
	if err != nil || len(f.writes) != 0 || strings.Contains(output.String(), "private-source") {
		t.Fatal(output.String(), err)
	}
	var reply map[string]any
	if err := json.Unmarshal(output.Bytes(), &reply); err != nil || reply["behavior"] != "muffle" {
		t.Fatal(output.String(), err)
	}
}

func TestStateLockExcludesConcurrentBridges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	first, err := lockState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := lockState(path); err == nil {
		second.Close()
		t.Fatal("second bridge acquired lock")
	}
	first.Close()
	third, err := lockState(path)
	if err != nil {
		t.Fatal("lock not released", err)
	}
	third.Close()
}

func TestJournaldLogsOmitTimestamp(t *testing.T) {
	for _, journald := range []bool{false, true} {
		var output bytes.Buffer
		newLogger(&output, journald).Info("Paused Sonos", "room", "Office")
		if got := strings.Contains(output.String(), "time="); got == journald {
			t.Errorf("journald=%v: %q", journald, output.String())
		}
		if !strings.Contains(output.String(), `msg="Paused Sonos" room=Office`) {
			t.Errorf("journald=%v: %q", journald, output.String())
		}
	}
}

func TestUnderJournaldMatchesStream(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	for stream, want := range map[string]bool{
		"":                                   false,
		"1:2":                                false,
		fmt.Sprintf("%d:%d", st.Dev, st.Ino): true,
	} {
		t.Setenv("JOURNAL_STREAM", stream)
		if got := underJournald(f); got != want {
			t.Errorf("JOURNAL_STREAM=%q: got %v", stream, got)
		}
	}
}

func TestHelpAndVersionDoNotRequireSonos(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, args := range [][]string{{"--help"}, {"--version"}} {
		var output bytes.Buffer
		if err := execute(args, &output, &output, testLogger()); err != nil {
			t.Fatal(err)
		}
		if scanner := bufio.NewScanner(&output); !scanner.Scan() {
			t.Fatal("empty help or version")
		}
	}
}
