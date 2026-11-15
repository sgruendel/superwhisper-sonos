package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

type desiredState struct {
	Active   bool
	Session  int64
	Behavior string
}

// The monitor owns only desired state. The worker serializes all Sonos writes
// and journal updates; slow network requests never block recording detection.
type monitor struct {
	whisper           whisperClient
	log               *slog.Logger
	pollInterval      time.Duration
	reconnectInterval time.Duration
	events            chan struct{}
	mu                sync.RWMutex
	desired           desiredState
}

func newMonitor(whisper whisperClient, log *slog.Logger) *monitor {
	return &monitor{whisper: whisper, log: log, pollInterval: 200 * time.Millisecond,
		reconnectInterval: time.Second, events: make(chan struct{}, 1)}
}

func (m *monitor) snapshot() desiredState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.desired
}

func (m *monitor) set(desired desiredState) {
	m.mu.Lock()
	changed := m.desired != desired
	m.desired = desired
	m.mu.Unlock()
	if changed {
		select {
		case m.events <- struct{}{}:
		default:
		}
	}
}

func (m *monitor) current(ctx context.Context, desired desiredState) bool {
	return ctx.Err() == nil && m.snapshot() == desired
}

func (m *monitor) run(ctx context.Context) {
	defer m.set(desiredState{})
	var last desiredState
	failed := false
	for ctx.Err() == nil {
		next, err := m.read(ctx, last)
		interval := m.pollInterval
		if err != nil {
			last = desiredState{}
			m.set(last)
			interval = m.reconnectInterval
			if !failed && ctx.Err() == nil {
				m.log.Info("Waiting for Superwhisper; restoring any Sonos changes")
			}
			failed = true
		} else {
			last = next
			m.set(next)
			if failed {
				m.log.Info("Superwhisper reconnected")
			}
			failed = false
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (m *monitor) read(ctx context.Context, last desiredState) (desiredState, error) {
	status, err := m.whisper.Status(ctx)
	if err != nil {
		return desiredState{}, err
	}
	if status.Phase != "recording" {
		return desiredState{}, nil
	}
	if status.SessionID == nil {
		return desiredState{}, errors.New("Superwhisper returned no recording session ID")
	}
	if last.Active && last.Session == *status.SessionID {
		return last, nil
	}
	behavior, err := m.whisper.Behavior(ctx)
	if err != nil {
		return desiredState{}, err
	}
	return desiredState{Active: true, Session: *status.SessionID, Behavior: behavior}, nil
}

func runBridge(ctx context.Context, b *bridge, m *monitor, configLoader func() (config, error)) error {
	monitorCtx, cancelMonitor := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { m.run(monitorCtx) })
	defer func() {
		cancelMonitor()
		wg.Wait()
	}()
	cleanup := func() error {
		// Signal cancellation stops new commands but must not cancel restoration.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := b.restore(cleanupCtx)
		return err
	}
	restored, err := b.restore(ctx)
	if err != nil {
		b.log.Warn("Sonos restoration pending", "error", err)
	}
	handled := desiredState{}
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return cleanup()
		case <-m.events:
			if m.snapshot() == handled && restored {
				continue
			}
		case <-retry.C:
			if restored {
				continue
			}
		}
		desired := m.snapshot()
		restored, err = b.restore(ctx)
		if err != nil {
			b.log.Warn("Sonos restoration pending", "error", err)
		}
		handled = desired
		if !restored || !desired.Active || !m.current(ctx, desired) {
			continue
		}
		cfg, err := configLoader()
		if err != nil {
			b.log.Error("Cannot load bridge config", "error", err)
			continue
		}
		if err := b.begin(ctx, desired.Behavior, cfg, func() bool { return m.current(ctx, desired) }); err != nil {
			b.log.Warn("Could not adjust Sonos", "error", err)
		}
	}
}
