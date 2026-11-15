package main

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// phaseRecording is the Superwhisper phase while the microphone is open. Other
// phases (idle, processing, ...) all mean "not recording" to the bridge.
const phaseRecording = "recording"

// playbackBehavior is Superwhisper's "while recording" playback setting.
//
//go:generate go tool stringer -type=playbackBehavior -linecomment
type playbackBehavior int

// Each line comment is the setting's value in Superwhisper's config.
const (
	behaviorUnknown     playbackBehavior = iota // unknown
	behaviorKeepPlaying                         // keepPlaying
	behaviorPause                               // pause
	behaviorMuffle                              // muffle
	behaviorDuck                                // duck
	behaviorMute                                // mute
)

// behaviorFromText maps a Superwhisper setting to its behavior. A setting this
// bridge does not know, such as one added in a later Superwhisper release, is
// behaviorUnknown.
func behaviorFromText(text string) playbackBehavior {
	for b := behaviorKeepPlaying; b <= behaviorMute; b++ {
		if b.String() == text {
			return b
		}
	}
	return behaviorUnknown
}

type whisperStatus struct {
	Phase     string `json:"phase"`
	SessionID *int64 `json:"session_id"`
}

type whisperClient interface {
	Status(context.Context) (whisperStatus, error)
	Behavior(context.Context) (string, error)
}

type whisperSocket struct {
	path string
}

func (c *whisperSocket) call(ctx context.Context, command string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.path)
	if err != nil {
		return fmt.Errorf("connect to Superwhisper: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	// jsontext.Encoder ends each top-level value with the newline the protocol expects.
	if err := json.MarshalEncode(jsontext.NewEncoder(conn), struct {
		Version int    `json:"version"`
		Command string `json:"command"`
	}{1, command}); err != nil {
		return fmt.Errorf("send Superwhisper %s: %w", command, err)
	}
	const maxReply = 2 * 1024 * 1024
	line, err := bufio.NewReader(io.LimitReader(conn, maxReply+1)).ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read Superwhisper %s: %w", command, err)
	}
	if len(line) > maxReply {
		return fmt.Errorf("reply from Superwhisper exceeds %d bytes", maxReply)
	}
	var envelope map[string]jsontext.Value
	if err := json.Unmarshal(line, &envelope); err != nil {
		return fmt.Errorf("decode Superwhisper %s: %w", command, err)
	}
	if envelope == nil {
		return errors.New("no JSON object from Superwhisper")
	}
	if message := envelope["error"]; len(message) > 0 && string(message) != "null" {
		return fmt.Errorf("%s request rejected by Superwhisper: %s", command, errorText(message))
	}
	return json.Unmarshal(line, out)
}

// errorText returns Superwhisper's error message, shortened for the log.
// Superwhisper sends a string; anything else is shown as raw JSON.
func errorText(raw jsontext.Value) string {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		text = string(raw)
	}
	return shorten(text, 200)
}

// shorten cuts s to at most limit bytes plus an ellipsis, dropping a
// multi-byte character the cut splits.
func shorten(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "") + "…"
}

func (c *whisperSocket) Status(ctx context.Context) (whisperStatus, error) {
	var status whisperStatus
	if err := c.call(ctx, "status", &status); err != nil {
		return status, err
	}
	if status.Phase == "" || (status.Phase == phaseRecording && status.SessionID == nil) {
		return status, errors.New("incomplete recording state from Superwhisper")
	}
	return status, nil
}

func (c *whisperSocket) Behavior(ctx context.Context) (string, error) {
	var reply struct {
		Config struct {
			Behavior string `json:"defaultPlaybackBehavior"`
		} `json:"config"`
	}
	if err := c.call(ctx, "audio_get", &reply); err != nil {
		return "", err
	}
	if reply.Config.Behavior == "" {
		return "", errors.New("no playback preference from Superwhisper")
	}
	return reply.Config.Behavior, nil
}
