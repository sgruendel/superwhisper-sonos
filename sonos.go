package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type device struct {
	IP       string `json:"ip"`
	Name     string `json:"name"`
	UDN      string `json:"udn"`
	Location string `json:"location,omitempty"`
}

// transportState is a UPnP AVTransport state as Sonos reports it. The set is
// defined by Sonos, so values beyond the constants below pass through as-is.
type transportState string

const (
	statePlaying transportState = "PLAYING"
	statePaused  transportState = "PAUSED_PLAYBACK"
)

type speakerStatus struct {
	Device    device `json:"device"`
	Transport struct {
		State transportState `json:"State"`
	} `json:"transport"`
	Position struct {
		TrackURI string `json:"TrackURI"`
	} `json:"position"`
	Volume *int `json:"volume"`
}

type sonosClient interface {
	Status(context.Context, string) (speakerStatus, error)
	GroupVolume(context.Context, string) (int, error)
	SetGroupVolume(context.Context, string, int) error
	GroupMute(context.Context, string) (bool, error)
	SetGroupMute(context.Context, string, bool) error
	Pause(context.Context, string) error
	Play(context.Context, string) error
}

type sonosCLI struct {
	binary     string
	lookupHost func(context.Context, string) ([]string, error)
}

func (c *sonosCLI) call(ctx context.Context, target string, out any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	flag := "--name"
	if net.ParseIP(target) != nil {
		flag = "--ip"
	} else if !strings.ContainsAny(target, " \t\n") {
		// Sinuous also accepts hostnames. Resolve these before falling back to a
		// Sonos room name; sonoscli's --name option only searches room names.
		lookup := c.lookupHost
		if lookup == nil {
			lookup = net.DefaultResolver.LookupHost
		}
		dnsCtx, cancelDNS := context.WithTimeout(ctx, 500*time.Millisecond)
		addresses, err := lookup(dnsCtx, target)
		cancelDNS()
		if err == nil {
			for _, address := range addresses {
				if ip := net.ParseIP(address); ip != nil && ip.To4() != nil {
					target, flag = address, "--ip"
					break
				}
			}
		}
	}
	argv := []string{"--format", "json", "--timeout", "2s", flag, target}
	cmd := exec.CommandContext(ctx, c.binary, append(argv, args...)...)
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("sonos %s: %w", strings.Join(args, " "), ctx.Err())
		}
		detail := shorten(strings.TrimSpace(stderr.String()), 300)
		return fmt.Errorf("sonos %s: %w: %s", strings.Join(args, " "), err, detail)
	}
	if out == nil {
		if !jsontext.Value(stdout.Bytes()).IsValid() {
			return fmt.Errorf("sonos %s returned invalid JSON", strings.Join(args, " "))
		}
		return nil
	}
	if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
		return fmt.Errorf("decode sonos %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func (c *sonosCLI) Status(ctx context.Context, target string) (speakerStatus, error) {
	var status speakerStatus
	if err := c.call(ctx, target, &status, "status"); err != nil {
		return status, err
	}
	// sonoscli exits successfully with an empty status when no speaker answers.
	if status.Device.IP == "" && status.Device.UDN == "" {
		return status, errors.New("no Sonos speaker answered")
	}
	if net.ParseIP(status.Device.IP) == nil || status.Device.UDN == "" || status.Transport.State == "" {
		return status, errors.New("incomplete device or transport state from Sonos")
	}
	return status, nil
}

func (c *sonosCLI) GroupVolume(ctx context.Context, target string) (int, error) {
	var reply struct {
		Volume *int `json:"volume"`
	}
	if err := c.call(ctx, target, &reply, "group", "volume", "get"); err != nil {
		return 0, err
	}
	if reply.Volume == nil || *reply.Volume < 0 || *reply.Volume > 100 {
		return 0, errors.New("invalid volume from Sonos")
	}
	return *reply.Volume, nil
}

func (c *sonosCLI) SetGroupVolume(ctx context.Context, target string, volume int) error {
	if volume < 0 || volume > 100 {
		return errors.New("volume must be between 0 and 100")
	}
	return c.call(ctx, target, nil, "group", "volume", "set", strconv.Itoa(volume))
}

func (c *sonosCLI) GroupMute(ctx context.Context, target string) (bool, error) {
	var reply struct {
		Mute *bool `json:"mute"`
	}
	if err := c.call(ctx, target, &reply, "group", "mute", "get"); err != nil {
		return false, err
	}
	if reply.Mute == nil {
		return false, errors.New("no mute state from Sonos")
	}
	return *reply.Mute, nil
}

func (c *sonosCLI) SetGroupMute(ctx context.Context, target string, mute bool) error {
	state := "off"
	if mute {
		state = "on"
	}
	return c.call(ctx, target, nil, "group", "mute", state)
}

func (c *sonosCLI) Pause(ctx context.Context, target string) error {
	return c.call(ctx, target, nil, "pause")
}

func (c *sonosCLI) Play(ctx context.Context, target string) error {
	return c.call(ctx, target, nil, "play")
}
