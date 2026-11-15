package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// action is one entry in the on-disk recovery journal.
type action struct {
	IP   string `json:"ip"`
	UDN  string `json:"udn"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	URI  string `json:"uri"`
	Old  int    `json:"old,omitempty"`
	New  int    `json:"new,omitempty"`
}

type journal struct{ path string }

func (j journal) load() ([]action, error) {
	data, err := os.ReadFile(j.path)
	if os.IsNotExist(err) {
		return []action{}, nil
	}
	if err != nil {
		return nil, err
	}
	var actions []action
	if err := json.Unmarshal(data, &actions); err != nil {
		return nil, fmt.Errorf("read recovery journal: %w", err)
	}
	if actions == nil {
		return nil, fmt.Errorf("recovery journal must be an array")
	}
	for _, a := range actions {
		if net.ParseIP(a.IP) == nil || a.UDN == "" || (a.Kind != "pause" && a.Kind != "volume") {
			return nil, fmt.Errorf("recovery journal contains an invalid action")
		}
		if a.Kind == "volume" && (a.New < 1 || a.Old <= a.New || a.Old > 100) {
			return nil, fmt.Errorf("recovery journal contains invalid volumes")
		}
	}
	return actions, nil
}

func (j journal) save(actions []action) error {
	dir := filepath.Dir(j.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".recovery-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if actions == nil {
		actions = []action{}
	}
	if err := json.NewEncoder(f).Encode(actions); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), j.path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
