package app

import (
	"encoding/json"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// The home model, first step: each device has a name and a room in the words
// people use ("the kitchen robot", "the car"), shown instead of device ids
// (home-w42-eu principle 2). People, places and routines come later.

const maxNameLen = 40

// displayName is the device's name, or a readable default from its model.
// Call with a.mu held.
func (r *robot) displayName() string {
	if r.name != "" {
		return r.name
	}
	switch {
	case strings.HasPrefix(r.caps.Model, "stackchan"):
		return "Stackchan"
	case strings.HasPrefix(r.caps.Model, "tpbot"):
		return "TPBot car"
	}
	return r.id
}

func cleanName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	for utf8.RuneCountInString(s) > maxNameLen {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// handleName sets a device's name and room: {"name": "...", "room": "..."}.
// An empty name falls back to the default.
func (a *App) handleName(w http.ResponseWriter, r *http.Request) {
	session := a.session(w, r)
	id := r.PathValue("id")
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var body struct {
		Name string `json:"name"`
		Room string `json:"room"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		http.Error(w, `body must be {"name", "room"}`, http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rob := a.robots[id]
	if rob == nil || !a.canSee(session, id) {
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
		return
	}
	rob.name, rob.room = cleanName(body.Name), cleanName(body.Room)
	a.addEvent(rob, eventView{Robot: id, Name: "renamed", Data: map[string]any{"name": rob.displayName(), "room": rob.room}, Sent: true, TS: time.Now()})
	a.publishRobot(rob)
	a.requestSave()
	writeJSON(w, http.StatusOK, map[string]string{"name": rob.displayName(), "room": rob.room})
}
