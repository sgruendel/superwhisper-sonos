package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Tests use a stateful fake: a command may change a speaker and still return an
// error, just as a real speaker can apply a request before its response is lost.
type fakeSonos struct {
	mu           sync.Mutex
	status       speakerStatus
	volume       int
	writes       []string
	offline      bool
	failWrite    bool
	beforePause  func()
	beforeStatus func(context.Context)
}

func newFakeSonos() *fakeSonos {
	f := &fakeSonos{volume: 40}
	f.status.Device = device{IP: "192.0.2.1", Name: "Office", UDN: "room-1"}
	f.status.Transport.State = "PLAYING"
	f.status.Position.TrackURI = "track:1"
	return f
}

func (f *fakeSonos) Status(ctx context.Context, _ string) (speakerStatus, error) {
	if f.beforeStatus != nil {
		f.beforeStatus(ctx)
	}
	if err := ctx.Err(); err != nil {
		return speakerStatus{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.offline {
		return speakerStatus{}, errors.New("speaker offline")
	}
	return f.status, nil
}

func (f *fakeSonos) GroupVolume(context.Context, string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.offline {
		return 0, errors.New("speaker offline")
	}
	return f.volume, nil
}

func (f *fakeSonos) write(command string, change func()) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.offline {
		return errors.New("speaker offline")
	}
	f.writes = append(f.writes, command)
	change()
	if f.failWrite {
		return errors.New("response lost after command applied")
	}
	return nil
}

func (f *fakeSonos) SetGroupVolume(_ context.Context, _ string, volume int) error {
	return f.write("volume", func() { f.volume = volume })
}

func (f *fakeSonos) Pause(context.Context, string) error {
	if f.beforePause != nil {
		f.beforePause()
	}
	return f.write("pause", func() { f.status.Transport.State = "PAUSED_PLAYBACK" })
}

func (f *fakeSonos) Play(context.Context, string) error {
	return f.write("play", func() { f.status.Transport.State = "PLAYING" })
}

func fixture(t *testing.T) (*bridge, *fakeSonos, config) {
	t.Helper()
	f := newFakeSonos()
	b, err := newBridge(f, filepath.Join(t.TempDir(), "recovery.json"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return b, f, config{Devices: []string{"192.0.2.1"}, VolumeFactor: 0.25}
}

func begin(t *testing.T, b *bridge, mode string, cfg config) {
	t.Helper()
	if err := b.begin(context.Background(), mode, cfg, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
}

func restore(t *testing.T, b *bridge) {
	t.Helper()
	if done, err := b.restore(context.Background()); !done || err != nil {
		t.Fatalf("restore: done=%v, err=%v", done, err)
	}
}

func TestPauseAndResumeOnlyOnce(t *testing.T) {
	b, f, cfg := fixture(t)
	begin(t, b, "pause", cfg)
	begin(t, b, "pause", cfg)
	if f.status.Transport.State != "PAUSED_PLAYBACK" {
		t.Fatal("not paused")
	}
	restore(t, b)
	restore(t, b)
	if !reflect.DeepEqual(f.writes, []string{"pause", "play"}) {
		t.Fatal(f.writes)
	}
}

func TestInactiveSpeakersStaySilent(t *testing.T) {
	for _, state := range []string{"STOPPED", "PAUSED_PLAYBACK"} {
		for _, mode := range []string{"pause", "muffle", "duck"} {
			t.Run(state+"/"+mode, func(t *testing.T) {
				b, f, cfg := fixture(t)
				f.status.Transport.State = state
				begin(t, b, mode, cfg)
				restore(t, b)
				if len(f.writes) != 0 {
					t.Fatal(f.writes)
				}
			})
		}
	}
}

func TestMuffleAndDuckRestoreVolume(t *testing.T) {
	for _, mode := range []string{"muffle", "duck"} {
		t.Run(mode, func(t *testing.T) {
			b, f, cfg := fixture(t)
			begin(t, b, mode, cfg)
			if f.volume != 10 {
				t.Fatal(f.volume)
			}
			restore(t, b)
			if f.volume != 40 {
				t.Fatal(f.volume)
			}
		})
	}
}

func TestManualVolumeChangeWins(t *testing.T) {
	b, f, cfg := fixture(t)
	begin(t, b, "muffle", cfg)
	f.volume = 17
	restore(t, b)
	if f.volume != 17 {
		t.Fatal(f.volume)
	}
}

func TestManualPlaybackAndCoordinatorChangesWin(t *testing.T) {
	for _, field := range []string{"source", "stop", "coordinator", "resume", "empty source"} {
		t.Run(field, func(t *testing.T) {
			b, f, cfg := fixture(t)
			if field == "empty source" {
				f.status.Position.TrackURI = ""
			}
			begin(t, b, "pause", cfg)
			switch field {
			case "source":
				f.status.Position.TrackURI = "track:2"
			case "stop":
				f.status.Transport.State = "STOPPED"
			case "coordinator":
				f.status.Device.UDN = "different-room"
			case "resume":
				f.status.Transport.State = "PLAYING"
			}
			restore(t, b)
			if !reflect.DeepEqual(f.writes, []string{"pause"}) {
				t.Fatal(f.writes)
			}
		})
	}
}

func TestDuplicateGroupTargetsAdjustOnce(t *testing.T) {
	b, f, cfg := fixture(t)
	cfg.Devices = append(cfg.Devices, "Other member")
	begin(t, b, "muffle", cfg)
	if len(f.writes) != 1 || f.volume != 10 {
		t.Fatal(f.writes, f.volume)
	}
}

func TestReleaseDuringPreflightOrJournalingPreventsWrite(t *testing.T) {
	for _, releaseAt := range []int{2, 3} {
		b, f, cfg := fixture(t)
		calls := 0
		err := b.begin(context.Background(), "pause", cfg, func() bool {
			calls++
			return calls < releaseAt
		})
		if err != nil || len(f.writes) != 0 {
			t.Fatal(err, f.writes)
		}
		restore(t, b)
		if len(f.writes) != 0 {
			t.Fatal(f.writes)
		}
	}
}

func TestJournalPrecedesSpeakerWrite(t *testing.T) {
	b, f, cfg := fixture(t)
	f.beforePause = func() {
		actions, err := b.journal.load()
		if err != nil || len(actions) != 1 || actions[0].Kind != "pause" {
			t.Fatalf("journal before pause: %v, %v", actions, err)
		}
	}
	begin(t, b, "pause", cfg)
}

func TestJournalFailurePreventsSpeakerWrite(t *testing.T) {
	b, f, cfg := fixture(t)
	b.journal.path = filepath.Join(t.TempDir(), "existing-directory")
	if err := os.Mkdir(b.journal.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := b.begin(context.Background(), "pause", cfg, func() bool { return true }); err == nil {
		t.Fatal("expected journal failure")
	}
	if len(f.writes) != 0 || len(b.actions) != 0 {
		t.Fatal("unjournaled speaker change", f.writes, b.actions)
	}
}

func TestRestartRecoversUncertainWrites(t *testing.T) {
	b, f, cfg := fixture(t)
	f.failWrite = true
	if err := b.begin(context.Background(), "muffle", cfg, func() bool { return true }); err == nil {
		t.Fatal("expected uncertain write")
	}
	if f.volume != 10 {
		t.Fatal(f.volume)
	}
	f.failWrite = false
	restarted, err := newBridge(f, b.journal.path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	restore(t, restarted)
	if f.volume != 40 {
		t.Fatal(f.volume)
	}
	actions, err := b.journal.load()
	if err != nil || len(actions) != 0 {
		t.Fatal(actions, err)
	}
}

func TestOfflineRestorationRetainsJournalAndBlocksNewSessions(t *testing.T) {
	b, f, cfg := fixture(t)
	begin(t, b, "pause", cfg)
	f.offline = true
	if done, err := b.restore(context.Background()); done || err == nil {
		t.Fatal(done, err)
	}
	begin(t, b, "muffle", cfg)
	if len(f.writes) != 1 {
		t.Fatal(f.writes)
	}
	f.offline = false
	restore(t, b)
}

func TestLowVolumesAreNotRaised(t *testing.T) {
	for _, volume := range []int{0, 1} {
		b, f, cfg := fixture(t)
		f.volume = volume
		begin(t, b, "muffle", cfg)
		if len(f.writes) != 0 {
			t.Fatal(f.writes)
		}
	}
}

func TestKeepPlayingAndUnsupportedMuteDoNotWrite(t *testing.T) {
	for _, mode := range []string{"keepPlaying", "mute"} {
		b, f, cfg := fixture(t)
		begin(t, b, mode, cfg)
		if len(f.writes) != 0 {
			t.Fatal(f.writes)
		}
	}
}

func TestPersistedJournalRecoveryAndPrivatePermissions(t *testing.T) {
	b, f, _ := fixture(t)
	f.volume = 10
	data := `[{"ip":"192.0.2.1","udn":"room-1","name":"Office","kind":"volume","uri":"track:1","old":40,"new":10}]`
	if err := os.WriteFile(b.journal.path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := newBridge(f, b.journal.path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	restore(t, restarted)
	if f.volume != 40 {
		t.Fatal(f.volume)
	}
	info, err := os.Stat(b.journal.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(info, err)
	}
}

func TestMalformedJournalIsRejected(t *testing.T) {
	for _, data := range []string{"null", "{}", "invalid", `[{}]`,
		`[{"ip":"192.0.2.1","udn":"room-1","kind":"volume","new":0,"old":40}]`} {
		path := filepath.Join(t.TempDir(), "recovery.json")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := (journal{path}).load(); err == nil {
			t.Fatal("accepted invalid journal", data)
		}
	}
}

func TestRecoveryClearsJournalAsArray(t *testing.T) {
	b, _, cfg := fixture(t)
	begin(t, b, "pause", cfg)
	restore(t, b)
	data, err := os.ReadFile(b.journal.path)
	if err != nil {
		t.Fatal(err)
	}
	var actions []action
	if err := json.Unmarshal(data, &actions); err != nil || actions == nil || len(actions) != 0 {
		t.Fatal(string(data), err)
	}
}

func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for bridge")
}
