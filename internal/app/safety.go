package app

import (
	"slices"
	"time"
)

// Safety stop: no forward driving while the car's sonar sees something closer
// than the car's limit. It works on the raw car_echo_us telemetry (cm ≈ µs / 58),
// for any car worker: tpbot-bridge or a Stackchan hosting the car.
//
// Backward and turning on the spot stay allowed, so the car can always get away.
// An echo of 0 means no echo (nothing in range, a missed echo, or the sonar is off):
// it starts no stop, and ends a running one only after missedEchoGrace.

const (
	defaultSafetyCm = 10
	safetyClearCm   = 3                       // hysteresis: the episode ends this much past the limit
	sonarStale      = 1500 * time.Millisecond // older readings do not count
)

// missedEchoGrace: how long readings without an echo keep a safety episode on.
var missedEchoGrace = time.Second

var safetyChoicesCm = []int{0, 5, 10, 15, 20, 30} // 0 = off

func isCar(r *robot) bool { return slices.Contains(r.caps.Commands, "car_drive") }

// obstacleCm is the distance the safety stop acts on, or 0 when there is none
// (no fresh reading, no echo, or farther than the limit). Call with a.mu held.
func (r *robot) obstacleCm() float64 {
	if r.safetyCm <= 0 || time.Since(r.telemetryAt) > sonarStale {
		return 0
	}
	us := r.telemetry["car_echo_us"]
	if us > 0 {
		r.lastEchoCm, r.lastEchoAt = us/58, r.telemetryAt
	}
	cm := r.lastEchoCm
	if us <= 0 {
		// No echo: nothing in range, or an echo the sonar missed. Close up it misses
		// every other one (tested 2026-10-02), so a missed echo does not end an episode:
		// only a real reading past the limit, or no echo at all for missedEchoGrace.
		if !r.safetyActive || time.Since(r.lastEchoAt) > missedEchoGrace {
			return 0
		}
	}
	limit := float64(r.safetyCm)
	if r.safetyActive {
		limit += safetyClearCm
	}
	if cm >= limit {
		return 0
	}
	return cm
}

func forward(left, right float64) bool { return left+right > 0 }

// guardDrive changes a forward car_drive into car_stop while there is an
// obstacle. Call with a.mu held. It returns the command to send instead and
// the distance, or ok=false when nothing changes.
func (a *App) guardDrive(r *robot, command string, args map[string]any) (string, map[string]any, float64, bool) {
	if command != "car_drive" || !isCar(r) {
		return command, args, 0, false
	}
	l, _ := args["left"].(float64)
	rt, _ := args["right"].(float64)
	cm := r.obstacleCm()
	a.setSafety(r, cm)
	if cm == 0 || !forward(l, rt) {
		return command, args, 0, false
	}
	return "car_stop", nil, cm, true
}

// checkSafety runs on every car telemetry: a car moving forward towards an
// obstacle stops now, not at the next car_drive. Call with a.mu held.
func (a *App) checkSafety(r *robot) {
	if !isCar(r) {
		return
	}
	cm := r.obstacleCm()
	a.setSafety(r, cm)
	if cm > 0 && r.conn != nil && forward(r.telemetry["car_left"], r.telemetry["car_right"]) {
		r.conn.command("car_stop", nil)
		a.hubCommand(r, "server:safety", "car_stop", nil, r.hub.lastID)
	}
}

// setSafety tracks the episode: one safety_stop event when it starts, and a
// fresh view when it starts or ends. Call with a.mu held.
func (a *App) setSafety(r *robot, cm float64) {
	active := cm > 0
	if active == r.safetyActive {
		return
	}
	r.safetyActive = active
	a.hubSafety(r, active, cm)
	if active {
		a.log.Info("safety stop", "robot", r.id, "cm", cm, "limit_cm", r.safetyCm)
		a.addEvent(r, eventView{Robot: r.id, Name: "safety_stop", Data: map[string]any{"cm": int(cm + 0.5), "limit_cm": r.safetyCm}, TS: time.Now()})
	}
	a.publishRobot(r)
}
