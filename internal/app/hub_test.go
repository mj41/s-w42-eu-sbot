package app

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mj41/sbot/internal/hub"
	"github.com/mj41/sbot/internal/loops"
	"github.com/mj41/stackchan-server/wire"
)

// A Stackchan hosting the car, as with car_enable: car commands and a face.
var hostedCaps = wire.RobotCapabilities{Model: "stackchan-cores3", Commands: []string{"car_drive", "car_stop", "emotion", "nod"}}

func newHubApp(t *testing.T, grants map[string][]string) (*App, *httptest.Server, *hub.Hub) {
	t.Helper()
	h, err := hub.Start(hub.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	a := New(Config{RobotToken: testToken, Hub: h, LoopGrants: grants, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if _, err := a.ServeLoopRequests(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler())
	a.cfg.PublicURL = srv.URL
	t.Cleanup(srv.Close)
	return a, srv, h
}

// nextCommandNot returns the next command other than the ones listed (sprite_clear, face on attach).
func (w *worker) nextCommandNot(skip ...string) wire.RobotCommandBody {
	w.t.Helper()
	for {
		c := w.nextCommand()
		keep := true
		for _, s := range skip {
			if c.Command == s {
				keep = false
			}
		}
		if keep {
			return c
		}
	}
}

func TestFrownLoopThroughTheHub(t *testing.T) {
	_, srv, h := newHubApp(t, map[string][]string{"frown": {"emotion"}})
	sc := dialWorker(t, srv, "stackchan-1", hostedCaps, nil)
	sc.next(wire.KindAccepted)
	start := time.Now().Add(-time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &loops.Runner{Hub: h, Mode: loops.Live, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	go r.Run(ctx, &loops.Frown{}, time.Time{})
	time.Sleep(200 * time.Millisecond) // the loop's consumer is ready

	sc.telemetry(map[string]float64{"car_echo_us": 58 * 6, "car_left": 0, "car_right": 0}) // 6 cm: blocked
	if c := sc.nextCommandNot("sprite_clear", "face"); c.Command != "emotion" || c.Args["name"] != "sad" {
		t.Fatalf("want emotion sad from the frown loop, got %+v", c)
	}
	sc.telemetry(map[string]float64{"car_echo_us": 58 * 40}) // 40 cm: clear
	if c := sc.nextCommandNot("sprite_clear", "face"); c.Command != "emotion" || c.Args["name"] != "neutral" {
		t.Fatalf("want emotion neutral, got %+v", c)
	}
	cancel()

	// Replay: the same loop over the recorded decisions sends nothing and reports both.
	time.Sleep(200 * time.Millisecond)
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	replay := &loops.Runner{Hub: h, Mode: loops.Replay, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	actions, err := replay.Run(rctx, &loops.Frown{}, start)
	if err != nil {
		t.Fatal(err)
	}
	var cmds []string
	for _, a := range actions {
		if a.Kind == "command" {
			cmds = append(cmds, a.Data["name"].(string))
		}
	}
	if len(cmds) != 2 || cmds[0] != "sad" || cmds[1] != "neutral" {
		t.Fatalf("replay: %v (actions %+v)", cmds, actions)
	}
	for _, a := range actions {
		if a.Cause == "" {
			t.Fatalf("replay action without a cause: %+v", a)
		}
	}
}

func TestLoopWithoutGrantIsRefused(t *testing.T) {
	a, srv, _ := newHubApp(t, nil)
	sc := dialWorker(t, srv, "stackchan-1", hostedCaps, nil)
	sc.next(wire.KindAccepted)
	reply := a.loopCommand(CommandRequest{Device: "stackchan-1", Command: "emotion", Args: map[string]any{"name": "sad"}, Source: "loop:frown"})
	if reply.OK || reply.Reason == "" {
		t.Fatalf("a loop without a grant got %+v", reply)
	}
	reply = a.loopCommand(CommandRequest{Device: "stackchan-1", Command: "emotion", Source: "browser:x"})
	if reply.OK {
		t.Fatalf("a non-loop source got %+v", reply)
	}
}

// Close up the sonar misses every other echo (seen on the TPBot, 2026-10-02): the
// readings alternate between ~8.8 cm and 0. That must be one episode, one "sad".
func TestMissedEchoesDoNotFlap(t *testing.T) {
	_, srv, h := newHubApp(t, map[string][]string{"frown": {"emotion"}})
	sc := dialWorker(t, srv, "stackchan-1", hostedCaps, nil)
	sc.next(wire.KindAccepted)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &loops.Runner{Hub: h, Mode: loops.Live, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	go r.Run(ctx, &loops.Frown{}, time.Time{})
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 6; i++ {
		us := 58 * 8.8
		if i%2 == 1 {
			us = 0
		}
		sc.telemetry(map[string]float64{"car_echo_us": us, "car_left": 0, "car_right": 0})
		time.Sleep(50 * time.Millisecond)
	}
	if c := sc.nextCommandNot("sprite_clear", "face"); c.Command != "emotion" || c.Args["name"] != "sad" {
		t.Fatalf("want one emotion sad, got %+v", c)
	}
	select {
	case f := <-sc.frames:
		if f.Kind == wire.KindRobotCommand {
			var c wire.RobotCommandBody
			f.Decode(&c)
			if c.Command == "emotion" {
				t.Fatalf("flapping: a second emotion %+v", c)
			}
		}
	case <-time.After(500 * time.Millisecond):
	}
}
