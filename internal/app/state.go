package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/mj41/stackchan-server/wire"
)

// The state file keeps pairings and known robots across restarts. It holds
// session ids, so it is mode 0600.

type savedState struct {
	Version  int                        `json:"version"`
	Sessions map[string]map[string]bool `json:"sessions"`
	Robots   []savedRobot               `json:"robots"`
}

type savedRobot struct {
	ID       string                 `json:"id"`
	Caps     wire.RobotCapabilities `json:"capabilities"`
	With     string                 `json:"with,omitempty"`
	LastSeen time.Time              `json:"last_seen"`
	SafetyCm *int                   `json:"safety_cm,omitempty"`
	Name     string                 `json:"name,omitempty"`
	Room     string                 `json:"room,omitempty"`
}

func (a *App) loadState() error {
	b, err := os.ReadFile(a.cfg.StateFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var st savedState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	for s, ids := range st.Sessions {
		if validSessionID(s) {
			a.sessions[s] = ids
		}
	}
	for _, sr := range st.Robots {
		r := a.robotFor(sr.ID)
		r.caps, r.with, r.lastSeen = sr.Caps, sr.With, sr.LastSeen
		r.name, r.room = sr.Name, sr.Room
		if sr.SafetyCm != nil {
			r.safetyCm = *sr.SafetyCm
		}
	}
	return nil
}

// requestSave asks RunStateSaver to write the file soon. Safe with a.mu held.
func (a *App) requestSave() {
	select {
	case a.saveNow <- struct{}{}:
	default:
	}
}

func (a *App) RunStateSaver(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.saveNow:
			time.Sleep(time.Second) // gather a burst of changes
			if err := a.SaveState(); err != nil {
				a.log.Warn("state not saved", "file", a.cfg.StateFile, "err", err)
			}
		}
	}
}

func (a *App) SaveState() error {
	if a.cfg.StateFile == "" {
		return nil
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	a.mu.Lock()
	st := savedState{Version: 1, Sessions: map[string]map[string]bool{}}
	for s, ids := range a.sessions {
		cp := map[string]bool{}
		for id := range ids {
			cp[id] = true
		}
		st.Sessions[s] = cp
	}
	for _, r := range a.robots {
		cm := r.safetyCm
		st.Robots = append(st.Robots, savedRobot{ID: r.id, Caps: r.caps, With: r.with, LastSeen: r.lastSeen, SafetyCm: &cm, Name: r.name, Room: r.room})
	}
	a.mu.Unlock()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.cfg.StateFile), 0o700); err != nil {
		return err
	}
	tmp := a.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.cfg.StateFile)
}
