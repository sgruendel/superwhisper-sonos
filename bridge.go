package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
)

type bridge struct {
	sonos   sonosClient
	journal journal
	actions []action
	log     *slog.Logger
}

func newBridge(sonos sonosClient, statePath string, log *slog.Logger) (*bridge, error) {
	j := journal{statePath}
	actions, err := j.load()
	if err != nil {
		return nil, err
	}
	return &bridge{sonos: sonos, journal: j, actions: actions, log: log}, nil
}

func (b *bridge) save(actions []action) error {
	if err := b.journal.save(actions); err != nil {
		return fmt.Errorf("save recovery journal: %w", err)
	}
	b.actions = actions
	return nil
}

func (b *bridge) begin(ctx context.Context, behavior playbackBehavior, cfg config, current func() bool) error {
	if len(b.actions) != 0 {
		return nil
	}
	switch behavior {
	case behaviorPause, behaviorMuffle, behaviorDuck, behaviorMute:
	default:
		// Keep Playing, or a setting the monitor already reported as unknown.
		return nil
	}
	seen := make(map[string]bool)
	var failures []error
	for _, target := range cfg.Devices {
		if ctx.Err() != nil || !current() {
			break
		}
		err := b.adjust(ctx, target, behavior, cfg.VolumeFactor, seen, current)
		if err != nil {
			failures = append(failures, fmt.Errorf("adjust %s: %w", target, err))
		}
	}
	return errors.Join(failures...)
}

func (b *bridge) adjust(ctx context.Context, target string, behavior playbackBehavior, factor float64,
	seen map[string]bool, current func() bool,
) error {
	status, err := b.sonos.Status(ctx, target)
	if err != nil {
		return err
	}
	ip := status.Device.IP
	if seen[ip] {
		return nil
	}
	seen[ip] = true
	if status.Transport.State != statePlaying {
		return nil
	}
	a := action{IP: ip, UDN: status.Device.UDN, Name: status.Device.Name,
		Kind: kindPause, URI: status.Position.TrackURI}
	switch behavior {
	case behaviorMute:
		// A group the user already muted stays theirs to unmute.
		a.Kind = kindMute
		muted, err := b.sonos.GroupMute(ctx, ip)
		if err != nil || muted {
			return err
		}
	case behaviorMuffle, behaviorDuck:
		a.Kind = kindVolume
		a.Old, err = b.sonos.GroupVolume(ctx, ip)
		if err != nil {
			return err
		}
		a.New = max(1, int(float64(a.Old)*factor))
		if a.New >= a.Old {
			return nil
		}
	}
	if ctx.Err() != nil || !current() {
		return nil
	}
	// Persist before sending the command. A failed request can still have
	// changed the speaker, so retain the journal even on a command error.
	if err := b.save(append(slices.Clone(b.actions), a)); err != nil {
		return err
	}
	if ctx.Err() != nil || !current() {
		return nil
	}
	switch a.Kind {
	case kindPause:
		if err := b.sonos.Pause(ctx, ip); err != nil {
			return err
		}
		b.log.Info("Paused Sonos", "room", a.Name)
	case kindMute:
		if err := b.sonos.SetGroupMute(ctx, ip, true); err != nil {
			return err
		}
		b.log.Info("Muted Sonos", "room", a.Name)
	case kindVolume:
		if err := b.sonos.SetGroupVolume(ctx, ip, a.New); err != nil {
			return err
		}
		b.log.Info("Lowered Sonos volume", "room", a.Name, "from", a.Old, "to", a.New)
	}
	return nil
}

func (b *bridge) restore(ctx context.Context) (bool, error) {
	var failures []error
	for _, a := range slices.Clone(b.actions) {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		if err := b.restoreOne(ctx, a); err != nil {
			failures = append(failures, fmt.Errorf("restore %s: %w", a.Name, err))
			continue
		}
		pending := slices.DeleteFunc(slices.Clone(b.actions), func(candidate action) bool { return candidate == a })
		if err := b.save(pending); err != nil {
			failures = append(failures, err)
		}
	}
	return len(b.actions) == 0, errors.Join(failures...)
}

func (b *bridge) restoreOne(ctx context.Context, a action) error {
	status, err := b.sonos.Status(ctx, a.IP)
	if err != nil {
		return err
	}
	if status.Device.UDN != a.UDN {
		b.log.Info("Coordinator changed; leaving playback alone", "room", a.Name)
		return nil
	}
	switch a.Kind {
	case kindMute:
		muted, err := b.sonos.GroupMute(ctx, a.IP)
		if err != nil {
			return err
		}
		if !muted {
			b.log.Info("Group unmuted manually; leaving it", "room", a.Name)
			return nil
		}
		if err := b.sonos.SetGroupMute(ctx, a.IP, false); err != nil {
			return err
		}
		b.log.Info("Unmuted Sonos", "room", a.Name)
		return nil
	case kindPause:
		if status.Transport.State == statePaused && a.URI != "" && status.Position.TrackURI == a.URI {
			if err := b.sonos.Play(ctx, a.IP); err != nil {
				return err
			}
			b.log.Info("Resumed Sonos", "room", a.Name)
		}
		return nil
	}
	volume, err := b.sonos.GroupVolume(ctx, a.IP)
	if err != nil {
		return err
	}
	if volume != a.New {
		b.log.Info("Volume changed manually; keeping it", "room", a.Name, "volume", volume)
		return nil
	}
	if err := b.sonos.SetGroupVolume(ctx, a.IP, a.Old); err != nil {
		return err
	}
	b.log.Info("Restored Sonos volume", "room", a.Name, "volume", a.Old)
	return nil
}
