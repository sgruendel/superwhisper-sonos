package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeWhisper struct {
	mu            sync.Mutex
	phase         string
	session       int64
	behavior      string
	offline       bool
	failNext      int
	statusReads   int
	behaviorReads int
}

func (f *fakeWhisper) Status(context.Context) (whisperStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusReads++
	if f.offline || f.failNext > 0 {
		f.failNext = max(0, f.failNext-1)
		return whisperStatus{}, errors.New("disconnected")
	}
	session := f.session
	return whisperStatus{Phase: f.phase, SessionID: &session}, nil
}

func (f *fakeWhisper) Behavior(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behaviorReads++
	return f.behavior, nil
}

func (f *fakeWhisper) set(phase string, session int64, behavior string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phase, f.session, f.behavior = phase, session, behavior
}

func fastMonitor(f *fakeWhisper) *monitor {
	m := newMonitor(f, testLogger())
	m.pollInterval = 2 * time.Millisecond
	m.reconnectInterval = 2 * time.Millisecond
	return m
}

func TestMonitorReadsPreferencePerSessionAndHandlesDisconnect(t *testing.T) {
	f := &fakeWhisper{phase: "recording", session: 1, behavior: "pause"}
	m := fastMonitor(f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, func() bool { return m.snapshot() == (desiredState{true, 1, "pause"}) })
	f.set("recording", 1, "muffle")
	if next, err := m.read(ctx, m.snapshot()); err != nil || next.Behavior != "pause" {
		t.Fatal("preference changed within recording", next, err)
	}
	f.set("processing", 1, "pause")
	waitFor(t, func() bool { return !m.snapshot().Active })
	f.set("recording", 2, "muffle")
	waitFor(t, func() bool { return m.snapshot() == (desiredState{true, 2, "muffle"}) })
	f.mu.Lock()
	if f.behaviorReads != 2 {
		t.Errorf("read preference %d times; want once per recording", f.behaviorReads)
	}
	f.offline = true
	f.mu.Unlock()
	waitFor(t, func() bool { return !m.snapshot().Active })
	f.mu.Lock()
	f.offline = false
	f.mu.Unlock()
	waitFor(t, func() bool { return m.snapshot().Active })
}

func TestMonitorToleratesTransientFailures(t *testing.T) {
	f := &fakeWhisper{phase: "recording", session: 1, behavior: "pause"}
	m := fastMonitor(f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	want := desiredState{true, 1, "pause"}
	waitFor(t, func() bool { return m.snapshot() == want })
	f.mu.Lock()
	f.failNext = m.lostAfter - 1
	start := f.statusReads
	f.mu.Unlock()
	// Wait for the failures plus two successful polls.
	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.statusReads >= start+m.lostAfter+1
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	// Clearing the session would have re-read the preference for it.
	if m.snapshot() != want || f.behaviorReads != 1 {
		t.Fatal("transient failures ended the recording", m.snapshot(), f.behaviorReads)
	}
}

func startTestWorker(t *testing.T, b *bridge, m *monitor, cfg config) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runBridge(ctx, b, m, func() (config, error) { return cfg, nil }) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(3 * time.Second):
				t.Error("worker did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func writeCount(f *fakeSonos) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func TestRecordingLifecycle(t *testing.T) {
	for _, mode := range []string{"pause", "muffle", "duck", "mute"} {
		for _, finish := range []string{"idle", "processing", "shutdown", "disconnect"} {
			t.Run(mode+"/"+finish, func(t *testing.T) {
				b, f, cfg := fixture(t)
				w := &fakeWhisper{phase: "idle"}
				m := fastMonitor(w)
				stop := startTestWorker(t, b, m, cfg)
				w.set("recording", 1, mode)
				waitFor(t, func() bool { return writeCount(f) == 1 })
				switch finish {
				case "shutdown":
					stop()
				case "disconnect":
					w.mu.Lock()
					w.offline = true
					w.mu.Unlock()
				default:
					w.set(finish, 1, mode)
				}
				waitFor(t, func() bool { return writeCount(f) == 2 })
				stop()
				if f.volume != 40 || f.muted || f.status.Transport.State != "PLAYING" || len(b.actions) != 0 {
					t.Fatal("original state was not restored", f.volume, f.muted, f.status, b.actions)
				}
			})
		}
	}
}

func TestSlowSonosDoesNotBlockReleaseDetection(t *testing.T) {
	b, f, cfg := fixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.beforeStatus = func(ctx context.Context) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	w := &fakeWhisper{phase: "recording", session: 1, behavior: "pause"}
	m := fastMonitor(w)
	stop := startTestWorker(t, b, m, cfg)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("Sonos request never started")
	}
	w.set("idle", 1, "pause")
	waitFor(t, func() bool { return !m.snapshot().Active })
	close(release)
	stop()
	if len(f.writes) != 0 {
		t.Fatal("late adjustment after recording ended", f.writes)
	}
}

func TestStartupRecoversBeforeNewSession(t *testing.T) {
	b, f, cfg := fixture(t)
	begin(t, b, "pause", cfg)
	restarted, err := newBridge(f, b.journal.path, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	w := &fakeWhisper{phase: "idle"}
	stop := startTestWorker(t, restarted, fastMonitor(w), cfg)
	waitFor(t, func() bool { return writeCount(f) == 2 })
	stop()
	if f.status.Transport.State != "PLAYING" || len(restarted.actions) != 0 {
		t.Fatal("startup recovery failed")
	}
}
