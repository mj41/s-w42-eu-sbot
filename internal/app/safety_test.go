package app

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mj41/stackchan-server/wire"
)

func (w *worker) telemetry(m map[string]float64) {
	w.t.Helper()
	f, _ := wire.Marshal(wire.KindRobotTelemetry, wire.Meta{}, wire.RobotTelemetryBody{Measurements: m})
	if err := w.ws.WriteMessage(websocket.TextMessage, f); err != nil {
		w.t.Fatal(err)
	}
}

func TestSafetyStop(t *testing.T) {
	a, srv := newTestApp(t, "")
	car := dialWorker(t, srv, "tpbot-1", carCaps, nil)
	owner := browser(t)
	pair(t, owner, srv, car)

	// sync waits until sbot has handled everything the car sent before.
	marker := 0.0
	sync := func() {
		t.Helper()
		marker++
		car.telemetry(map[string]float64{"car_uptime_ms": marker})
		for range 100 {
			a.mu.Lock()
			done := a.robots["tpbot-1"].telemetry["car_uptime_ms"] == marker
			a.mu.Unlock()
			if done {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("telemetry not handled")
	}
	drive := func(l, r int) wire.RobotCommandBody {
		t.Helper()
		body := `{"command":"car_drive","args":{"left":` + strconv.Itoa(l) + `,"right":` + strconv.Itoa(r) + `}}`
		if s := post(t, owner, srv, "tpbot-1", body); s != http.StatusAccepted {
			t.Fatalf("drive: %d", s)
		}
		return car.nextCommand()
	}

	car.telemetry(map[string]float64{"car_echo_us": 58 * 30, "car_left": 0, "car_right": 0}) // 30 cm
	sync()
	if c := drive(50, 50); c.Command != "car_drive" {
		t.Fatalf("far away: got %s", c.Command)
	}

	car.telemetry(map[string]float64{"car_echo_us": 58 * 8}) // 8 cm, the car is stopped
	sync()
	if c := drive(50, 50); c.Command != "car_stop" {
		t.Fatalf("forward at 8 cm: got %s", c.Command)
	}
	if c := drive(40, 10); c.Command != "car_stop" {
		t.Fatalf("forward curve at 8 cm: got %s", c.Command)
	}
	if c := drive(-40, -40); c.Command != "car_drive" {
		t.Fatalf("backward at 8 cm: got %s", c.Command)
	}
	if c := drive(-30, 30); c.Command != "car_drive" {
		t.Fatalf("turn on the spot at 8 cm: got %s", c.Command)
	}

	// The car reports it is moving forward at 8 cm: sbot stops it at once.
	car.telemetry(map[string]float64{"car_echo_us": 58 * 8, "car_left": 40, "car_right": 40})
	if c := car.nextCommand(); c.Command != "car_stop" {
		t.Fatalf("telemetry stop: got %s", c.Command)
	}

	// Hysteresis: 11 cm is still blocked (limit 10 + 3), 14 cm is free again.
	car.telemetry(map[string]float64{"car_echo_us": 58 * 11, "car_left": 0, "car_right": 0})
	sync()
	if c := drive(50, 50); c.Command != "car_stop" {
		t.Fatalf("forward at 11 cm during the episode: got %s", c.Command)
	}
	car.telemetry(map[string]float64{"car_echo_us": 58 * 14})
	sync()
	if c := drive(50, 50); c.Command != "car_drive" {
		t.Fatalf("forward at 14 cm: got %s", c.Command)
	}

	// A missed echo during an episode does not end it; no echo for longer does.
	missedEchoGrace = 300 * time.Millisecond
	defer func() { missedEchoGrace = time.Second }()
	car.telemetry(map[string]float64{"car_echo_us": 58 * 8})
	sync()
	car.telemetry(map[string]float64{"car_echo_us": 0})
	sync()
	if c := drive(50, 50); c.Command != "car_stop" {
		t.Fatalf("missed echo during an episode: got %s, want car_stop", c.Command)
	}
	time.Sleep(missedEchoGrace + 100*time.Millisecond)
	car.telemetry(map[string]float64{"car_echo_us": 0})
	sync()
	if c := drive(50, 50); c.Command != "car_drive" {
		t.Fatalf("no echo for longer than the grace: got %s", c.Command)
	}

	// No echo outside an episode (nothing in range, or the sonar is off): no stop.
	car.telemetry(map[string]float64{"car_echo_us": 0})
	sync()
	if c := drive(50, 50); c.Command != "car_drive" {
		t.Fatalf("no echo: got %s", c.Command)
	}

	// The limit can be switched off, and only to the offered values.
	setLimit := func(body string) int {
		resp, err := owner.Post(srv.URL+"/api/robots/tpbot-1/safety", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if s := setLimit(`{"cm":7}`); s != http.StatusBadRequest {
		t.Fatalf("cm 7: %d", s)
	}
	if s := setLimit(`{"cm":0}`); s != http.StatusOK {
		t.Fatalf("cm 0: %d", s)
	}
	car.telemetry(map[string]float64{"car_echo_us": 58 * 3})
	sync()
	if c := drive(50, 50); c.Command != "car_drive" {
		t.Fatalf("safety off at 3 cm: got %s", c.Command)
	}
	if v := listRobots(t, owner, srv); v[0].Safety == nil || v[0].Safety.LimitCm != 0 {
		t.Fatalf("view: %+v", v[0].Safety)
	}
}
