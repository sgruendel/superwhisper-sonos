package main

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net"
	"time"
)

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
		return fmt.Errorf("Superwhisper reply exceeds %d bytes", maxReply)
	}
	var envelope map[string]jsontext.Value
	if err := json.Unmarshal(line, &envelope); err != nil {
		return fmt.Errorf("decode Superwhisper %s: %w", command, err)
	}
	if envelope == nil {
		return fmt.Errorf("Superwhisper returned no object")
	}
	if message := envelope["error"]; len(message) > 0 && string(message) != "null" {
		return fmt.Errorf("Superwhisper rejected %s", command)
	}
	return json.Unmarshal(line, out)
}

func (c *whisperSocket) Status(ctx context.Context) (whisperStatus, error) {
	var status whisperStatus
	if err := c.call(ctx, "status", &status); err != nil {
		return status, err
	}
	if status.Phase == "" || (status.Phase == "recording" && status.SessionID == nil) {
		return status, fmt.Errorf("Superwhisper returned an incomplete recording state")
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
		return "", fmt.Errorf("Superwhisper returned no playback preference")
	}
	return reply.Config.Behavior, nil
}
