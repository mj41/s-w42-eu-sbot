package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/mj41/s-w42-eu-sbot/internal/hub"
	"github.com/nats-io/nats.go"
)

// sbot is the web/API server of the home node (home-w42-eu architecture §5). It writes
// everything devices send, every command and every safety decision into the event hub,
// and takes command requests from loops in the controller server (§7).

// Stored telemetry is downsampled: at most one merged message per device per interval.
// Every frame still goes to live.telemetry.<device> for live consumers.
const storedTelemetryEvery = time.Second

type hubState struct {
	lastStored time.Time
	pending    map[string]float64 // measurements since the last stored message
	lastID     string             // id of the last telemetry event: the cause of decisions
}

func (a *App) hubOn() bool { return a.cfg.Hub != nil }

// browserSource names a browser in the hub without its session id (a credential).
func browserSource(session string) string {
	sum := sha256.Sum256([]byte(session))
	return "browser:" + hex.EncodeToString(sum[:4])
}

// hubTelemetry publishes a telemetry frame. Call with a.mu held.
func (a *App) hubTelemetry(r *robot, m map[string]float64) {
	if !a.hubOn() {
		return
	}
	now := time.Now()
	ev := hub.Event{ID: hub.NewID(now), TS: now, Source: "device:" + r.id, Kind: "telemetry", Device: r.id, Data: toAny(m)}
	r.hub.lastID = ev.ID
	a.cfg.Hub.Publish("live.telemetry."+hub.Token(r.id), ev)
	if r.hub.pending == nil {
		r.hub.pending = map[string]float64{}
	}
	for k, v := range m {
		r.hub.pending[k] = v
	}
	if now.Sub(r.hub.lastStored) < storedTelemetryEvery {
		return
	}
	ev.Data = toAny(r.hub.pending)
	r.hub.pending, r.hub.lastStored = nil, now
	a.cfg.Hub.Publish("telemetry."+hub.Token(r.id), ev)
}

// hubEvent publishes a device event (or online/offline from the server).
func (a *App) hubEvent(r *robot, source, name string, data map[string]any) {
	if !a.hubOn() {
		return
	}
	now := time.Now()
	a.cfg.Hub.Publish("events."+hub.Token(r.id)+"."+hub.Token(name),
		hub.Event{ID: hub.NewID(now), TS: now, Source: source, Kind: "event", Device: r.id, Name: name, Data: data})
}

// hubCommand records a command sent to a device, with who sent it and why.
func (a *App) hubCommand(r *robot, source, command string, args map[string]any, cause string) {
	if !a.hubOn() {
		return
	}
	now := time.Now()
	a.cfg.Hub.Publish("commands."+hub.Token(r.id)+"."+hub.Token(command),
		hub.Event{ID: hub.NewID(now), TS: now, Source: source, Kind: "command", Device: r.id, Name: command, Data: args, Cause: cause})
}

// hubSafety records a change of the safety stop: on (with the distance) or off.
// The cause is the reading that changed it. face is the device that can show it.
func (a *App) hubSafety(r *robot, active bool, cm float64) {
	if !a.hubOn() {
		return
	}
	now := time.Now()
	data := map[string]any{"active": active, "cm": cm, "limit_cm": r.safetyCm}
	if face := a.faceFor(r); face != "" {
		data["face"] = face
	}
	a.cfg.Hub.Publish("decisions.safety."+hub.Token(r.id),
		hub.Event{ID: hub.NewID(now), TS: now, Source: "server:safety", Kind: "decision", Device: r.id, Name: "safety", Data: data, Cause: r.hub.lastID})
}

// faceFor is the device that can show a car's state with a face: the car itself when
// a robot hosts it, else the robot it is linked to. Call with a.mu held.
func (a *App) faceFor(r *robot) string {
	if slices.Contains(r.caps.Commands, "emotion") {
		return r.id
	}
	if w := a.robots[r.with]; w != nil && slices.Contains(w.caps.Commands, "emotion") {
		return w.id
	}
	return ""
}

func toAny(m map[string]float64) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

/* ------------------------------ loop requests ----------------------------- */

// CommandRequest is what a loop sends on requests.commands.<device>.
type CommandRequest struct {
	Device  string         `json:"device"`
	Command string         `json:"command"`
	Args    map[string]any `json:"args,omitempty"`
	Source  string         `json:"source"` // loop:<name>
	Cause   string         `json:"cause,omitempty"`
}

type CommandReply struct {
	OK     bool   `json:"ok"`
	Status string `json:"status,omitempty"` // sent, safety_stop
	Reason string `json:"reason,omitempty"`
}

// ServeLoopRequests answers command requests from the controller server.
func (a *App) ServeLoopRequests() (*nats.Subscription, error) {
	if !a.hubOn() {
		return nil, errors.New("no hub")
	}
	return a.cfg.Hub.NC.Subscribe("requests.commands.*", func(m *nats.Msg) {
		var req CommandRequest
		reply := CommandReply{}
		if err := json.Unmarshal(m.Data, &req); err != nil {
			reply.Reason = "bad request"
		} else {
			reply = a.loopCommand(req)
		}
		b, _ := json.Marshal(reply)
		m.Respond(b)
	})
}

// loopCommand applies the policy to a loop's command: the loop needs a grant for the
// command (restricted by default), and the same checks as a browser's command apply,
// including the safety stop.
func (a *App) loopCommand(req CommandRequest) CommandReply {
	loop, ok := strings.CutPrefix(req.Source, "loop:")
	if !ok || loop == "" {
		return CommandReply{Reason: "source must be loop:<name>"}
	}
	if !slices.Contains(a.cfg.LoopGrants[loop], req.Command) {
		a.log.Warn("loop command refused: no grant", "loop", loop, "command", req.Command, "robot", req.Device)
		return CommandReply{Reason: "no grant for " + req.Command}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rob := a.robots[req.Device]
	switch {
	case rob == nil:
		return CommandReply{Reason: "unknown device"}
	case serverOnly[req.Command] || !slices.Contains(rob.caps.Commands, req.Command):
		return CommandReply{Reason: "device does not take " + req.Command}
	case rob.conn == nil:
		return CommandReply{Reason: "device offline"}
	}
	name, args, _, limited := a.guardDrive(rob, req.Command, req.Args)
	if !rob.conn.command(name, args) {
		return CommandReply{Reason: "device busy"}
	}
	a.hubCommand(rob, req.Source, name, args, req.Cause)
	a.log.Info("loop command", "loop", loop, "robot", rob.id, "command", name)
	a.addEvent(rob, eventView{Robot: rob.id, Name: name, Data: map[string]any{"by": req.Source}, Sent: true, TS: time.Now()})
	if limited {
		return CommandReply{OK: true, Status: "safety_stop"}
	}
	return CommandReply{OK: true, Status: "sent"}
}
