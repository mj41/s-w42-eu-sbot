// Package loops runs loops and controllers as consumers of the event hub: the
// controller server (home-w42-eu architecture §7). A loop reads events and asks for
// commands; the web/API server's policy decides whether a command is sent.
//
// Every loop runs in one of three modes (§7.1):
//
//	replay  consume recorded events from a time in the past, send nothing, report
//	        what the loop would have done
//	shadow  consume live events, publish decisions only ("would command ...")
//	live    consume live events, request commands, publish decisions
package loops

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mj41/sbot/internal/hub"
)

// Loop is one behaviour: it gets the events on its subjects, in order.
type Loop interface {
	Name() string
	Subjects() []string
	Handle(ev hub.Event, out Out)
}

// Out is what a loop may do. cause is the id of the event that led to it.
type Out interface {
	Command(device, command string, args map[string]any, cause string)
	Decide(device, name string, data map[string]any, cause string)
}

type Mode string

const (
	Replay Mode = "replay"
	Shadow Mode = "shadow"
	Live   Mode = "live"
)

// Action is one thing a loop did or would have done, for replay reports and logs.
type Action struct {
	TS     time.Time      `json:"ts"`
	Kind   string         `json:"kind"` // command or decision
	Device string         `json:"device"`
	Name   string         `json:"name"`
	Data   map[string]any `json:"data,omitempty"`
	Cause  string         `json:"cause,omitempty"`
	Result string         `json:"result,omitempty"` // live commands: sent, safety_stop, or the reason it was refused
}

// Runner connects loops to the hub in one mode.
type Runner struct {
	Hub  *hub.Hub
	Mode Mode
	Log  *slog.Logger
}

// Run runs one loop until ctx ends. In replay mode it consumes from since until it
// has caught up, and returns what the loop would have done.
func (r *Runner) Run(ctx context.Context, l Loop, since time.Time) ([]Action, error) {
	out := &output{r: r, loop: l.Name()}
	var start time.Time
	untilNow := false
	if r.Mode == Replay {
		start, untilNow = since, true
	}
	err := r.Hub.Follow(ctx, l.Subjects(), start, untilNow, func(ev hub.Event) {
		out.now = ev.TS
		l.Handle(ev, out)
	})
	return out.actions, err
}

type output struct {
	r       *Runner
	loop    string
	now     time.Time // the time of the event being handled (replay: then, not now)
	actions []Action
}

func (o *output) Command(device, command string, args map[string]any, cause string) {
	a := Action{TS: o.now, Kind: "command", Device: device, Name: command, Data: args, Cause: cause}
	switch o.r.Mode {
	case Replay:
	case Shadow:
		o.publish(device, "would_command", map[string]any{"command": command, "args": args}, cause, true)
	case Live:
		a.Result = o.request(device, command, args, cause)
		o.r.Log.Info("loop command", "loop", o.loop, "device", device, "command", command, "result", a.Result)
	}
	o.actions = append(o.actions, a)
}

func (o *output) Decide(device, name string, data map[string]any, cause string) {
	o.actions = append(o.actions, Action{TS: o.now, Kind: "decision", Device: device, Name: name, Data: data, Cause: cause})
	if o.r.Mode != Replay {
		o.publish(device, name, data, cause, o.r.Mode == Shadow)
	}
}

func (o *output) publish(device, name string, data map[string]any, cause string, shadow bool) {
	d := map[string]any{}
	for k, v := range data {
		d[k] = v
	}
	if shadow {
		d["shadow"] = true
	}
	now := time.Now()
	o.r.Hub.Publish("decisions."+hub.Token(o.loop)+"."+hub.Token(device), hub.Event{
		ID: hub.NewID(now), TS: now, Source: "loop:" + o.loop, Kind: "decision", Device: device, Name: name, Data: d, Cause: cause,
	})
}

// request asks the web/API server to send a command; its policy decides.
func (o *output) request(device, command string, args map[string]any, cause string) string {
	b, _ := json.Marshal(map[string]any{"device": device, "command": command, "args": args, "source": "loop:" + o.loop, "cause": cause})
	msg, err := o.r.Hub.NC.Request("requests.commands."+hub.Token(device), b, 3*time.Second)
	if err != nil {
		return "no answer: " + err.Error()
	}
	var reply struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(msg.Data, &reply) != nil {
		return "bad answer"
	}
	if !reply.OK {
		return "refused: " + reply.Reason
	}
	return reply.Status
}

// Report formats replay actions for people.
func Report(loop string, since time.Time, actions []Action) string {
	s := fmt.Sprintf("%s, replayed since %s: %d actions\n", loop, since.Format(time.RFC3339), len(actions))
	for _, a := range actions {
		s += fmt.Sprintf("  %s  %-8s %s %s %v\n", a.TS.Local().Format("Jan 02 15:04:05"), a.Kind, a.Device, a.Name, a.Data)
	}
	return s
}
