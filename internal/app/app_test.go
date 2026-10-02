package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mj41/stackchan-server/wire"
)

const testToken = "test-token"

func newTestApp(t *testing.T, stateFile string) (*App, *httptest.Server) {
	t.Helper()
	a := New(Config{RobotToken: testToken, StateFile: stateFile, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	srv := httptest.NewServer(a.Handler())
	a.cfg.PublicURL = srv.URL
	t.Cleanup(srv.Close)
	return a, srv
}

// worker is a fake robot or bridge.
type worker struct {
	t      *testing.T
	ws     *websocket.Conn
	frames chan wire.Frame
	bins   chan []byte
}

func dialWorker(t *testing.T, srv *httptest.Server, id string, caps wire.RobotCapabilities, labels map[string]string) *worker {
	t.Helper()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+testToken)
	h.Set(wire.WorkerIDHeader, id)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+wire.ConnectPath, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	f, _ := wire.Marshal(wire.KindRegister, wire.Meta{}, wire.RegisterBody{Class: wire.ClassRobot, Capabilities: caps, Labels: labels})
	ws.WriteMessage(websocket.TextMessage, f)
	w := &worker{t: t, ws: ws, frames: make(chan wire.Frame, 64), bins: make(chan []byte, 8)}
	go func() {
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				w.bins <- data
				continue
			}
			fs, _ := wire.Parse(data)
			for _, f := range fs {
				w.frames <- f
			}
		}
	}()
	return w
}

// next returns the next frame of the kind, skipping others.
func (w *worker) next(kind string) wire.Frame {
	w.t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-w.frames:
			if f.Kind == kind {
				return f
			}
		case <-deadline:
			w.t.Fatalf("no %s frame", kind)
		}
	}
}

func (w *worker) nextCommand() wire.RobotCommandBody {
	w.t.Helper()
	var c wire.RobotCommandBody
	w.next(wire.KindRobotCommand).Decode(&c)
	return c
}

func browser(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func pair(t *testing.T, c *http.Client, srv *httptest.Server, w *worker) {
	t.Helper()
	var p wire.PairCodeBody
	w.next(wire.KindPairCode).Decode(&p)
	resp, err := c.Get(srv.URL + "/pair?code=" + url.QueryEscape(p.Code))
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("pair: %v %v", err, resp.Status)
	}
}

func listRobots(t *testing.T, c *http.Client, srv *httptest.Server) []robotView {
	t.Helper()
	resp, err := c.Get(srv.URL + "/api/robots")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v []robotView
	json.NewDecoder(resp.Body).Decode(&v)
	return v
}

func post(t *testing.T, c *http.Client, srv *httptest.Server, id, body string) int {
	t.Helper()
	resp, err := c.Post(srv.URL+"/api/robots/"+id+"/command", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

var (
	chanCaps = wire.RobotCapabilities{Model: "stackchan-cores3", Commands: []string{"nod", "camera", "sprite_clear", "face"}}
	carCaps  = wire.RobotCapabilities{Model: "tpbot-microbit", Commands: []string{"car_drive", "car_stop"}}
)

func TestCarBelongsToStackchan(t *testing.T) {
	_, srv := newTestApp(t, "")
	sc := dialWorker(t, srv, "stackchan-1", chanCaps, nil)
	sc.next(wire.KindAccepted)
	if c := sc.nextCommand(); c.Command != "sprite_clear" {
		t.Fatalf("want sprite_clear first, got %+v", c)
	}
	if c := sc.nextCommand(); c.Command != "face" {
		t.Fatalf("want face, got %+v", c)
	}
	car := dialWorker(t, srv, "tpbot-1a2b", carCaps, map[string]string{"with": "stackchan-1"})
	car.next(wire.KindAccepted)

	owner, stranger := browser(t), browser(t)
	pair(t, owner, srv, sc)
	sc.next(wire.KindPaired)

	got := listRobots(t, owner, srv)
	if len(got) != 2 || got[0].ID != "stackchan-1" || got[1].ID != "tpbot-1a2b" || !got[1].Car || got[1].With != "stackchan-1" || !got[0].Camera {
		t.Fatalf("owner sees %+v", got)
	}
	if v := listRobots(t, stranger, srv); len(v) != 0 {
		t.Fatalf("stranger sees %+v", v)
	}

	if s := post(t, owner, srv, "tpbot-1a2b", `{"command":"car_drive","args":{"left":50,"right":-50}}`); s != http.StatusAccepted {
		t.Fatalf("car_drive: %d", s)
	}
	if c := car.nextCommand(); c.Command != "car_drive" || c.Args["left"] != 50.0 || c.Args["right"] != -50.0 {
		t.Fatalf("car got %+v", c)
	}
	for _, c := range []struct {
		who  *http.Client
		id   string
		body string
		want int
	}{
		{stranger, "tpbot-1a2b", `{"command":"car_stop"}`, http.StatusForbidden},
		{owner, "stackchan-1", `{"command":"camera","args":{"on":true}}`, http.StatusBadRequest}, // server only
		{owner, "tpbot-1a2b", `{"command":"nod"}`, http.StatusBadRequest},                        // not in its capabilities
		{owner, "stackchan-1", `{"command":"nod"}`, http.StatusAccepted},
	} {
		if s := post(t, c.who, srv, c.id, c.body); s != c.want {
			t.Errorf("%s %s: %d, want %d", c.id, c.body, s, c.want)
		}
	}
}

func TestCameraOnlyWhileWatched(t *testing.T) {
	_, srv := newTestApp(t, "")
	sc := dialWorker(t, srv, "stackchan-1", chanCaps, nil)
	owner := browser(t)
	pair(t, owner, srv, sc)

	u, _ := url.Parse(srv.URL)
	h := http.Header{}
	h.Set("Origin", srv.URL)
	for _, c := range owner.Jar.Cookies(u) {
		h.Add("Cookie", c.Name+"="+c.Value)
	}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/robots/stackchan-1/media?video=1", h)
	if err != nil {
		t.Fatal(err)
	}
	if c := sc.nextCommand(); c.Command != "camera" || c.Args["on"] != true {
		t.Fatalf("want camera on, got %+v", c)
	}
	sc.ws.WriteMessage(websocket.BinaryMessage, []byte{wire.BinCameraJPEG, 0xFF, 0xD8})
	ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, msg, err := ws.ReadMessage(); err != nil || len(msg) != 3 || msg[0] != wire.BinCameraJPEG {
		t.Fatalf("frame: % x %v", msg, err)
	}
	ws.Close()
	if c := sc.nextCommand(); c.Command != "camera" || c.Args["on"] != false {
		t.Fatalf("want camera off, got %+v", c)
	}

	sc.ws.WriteMessage(websocket.BinaryMessage, []byte{wire.BinSnapshot, 0xFF, 0xD8, 0xFF})
	var body []byte
	for i := 0; i < 50 && len(body) != 3; i++ {
		time.Sleep(10 * time.Millisecond)
		if resp, err := owner.Get(srv.URL + "/api/robots/stackchan-1/snapshot"); err == nil {
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
	}
	if len(body) != 3 {
		t.Fatalf("snapshot: % x", body)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	a, srv := newTestApp(t, file)
	sc := dialWorker(t, srv, "stackchan-1", chanCaps, nil)
	owner := browser(t)
	pair(t, owner, srv, sc)
	if err := a.SaveState(); err != nil {
		t.Fatal(err)
	}

	b := New(Config{RobotToken: testToken, StateFile: file, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	srv2 := httptest.NewServer(b.Handler())
	defer srv2.Close()
	u, _ := url.Parse(srv.URL)
	u2, _ := url.Parse(srv2.URL)
	owner.Jar.SetCookies(u2, owner.Jar.Cookies(u))
	got := listRobots(t, owner, srv2)
	if len(got) != 1 || got[0].ID != "stackchan-1" || got[0].Online || got[0].Model != "stackchan-cores3" {
		t.Fatalf("after restart: %+v", got)
	}
}
