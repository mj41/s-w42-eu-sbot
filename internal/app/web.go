package app

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mj41/stackchan-server/wire"
)

//go:embed ui
var uiFS embed.FS

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	a.session(w, r)
	page, err := uiFS.ReadFile("ui/index.html")
	if a.cfg.UIDir != "" {
		page, err = os.ReadFile(filepath.Join(a.cfg.UIDir, "index.html"))
	}
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(page)
}

// handlePair is the QR target (GET, because a phone camera opens it).
// The one-time code is the proof that the user can see the robot.
func (a *App) handlePair(w http.ResponseWriter, r *http.Request) {
	session := a.session(w, r)
	code := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(r.URL.Query().Get("code")))

	a.mu.Lock()
	pc, ok := a.codes[code]
	if ok {
		delete(a.codes, code)
		ok = time.Now().Before(pc.expires)
	}
	var rob *robot
	if ok {
		if a.sessions[session] == nil {
			a.sessions[session] = map[string]bool{}
		}
		a.sessions[session][pc.robotID] = true
		a.requestSave()
		rob = a.robots[pc.robotID]
		if rob != nil && rob.conn != nil {
			rob.conn.frame(wire.KindPaired, wire.PairedBody{Viewers: a.viewers(rob.id)})
			rob.conn.frame(wire.KindPairCode, a.issueCode(rob.id)) // the QR on screen stays valid
		}
		for _, x := range a.robots {
			if x == rob || x.with == pc.robotID {
				a.publishRobot(x)
			}
		}
	}
	a.mu.Unlock()

	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">`+
			`<title>Pairing failed</title><body style="font-family:system-ui;padding:16px">`+
			`<h1>Pairing failed</h1><p>This code is invalid, expired, or already used. `+
			`Scan the QR code on the robot again.</p><p><a href="/">Open sbot</a></p>`)
		return
	}
	a.log.Info("browser paired", "robot", pc.robotID)
	http.Redirect(w, r, "/?paired="+url.QueryEscape(pc.robotID), http.StatusSeeOther)
}

func (a *App) handleListRobots(w http.ResponseWriter, r *http.Request) {
	session := a.session(w, r)
	a.mu.Lock()
	views := []robotView{}
	for _, x := range a.visible(session) {
		views = append(views, x.view())
	}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, views)
}

/* ----------------------------------- SSE ---------------------------------- */

type subscriber struct {
	session string
	events  chan sseEvent
}

type sseEvent struct {
	name string
	data []byte
}

// publish sends an event about a robot to every browser that can see it.
// Call with a.mu held. A slow browser loses events rather than blocking robots.
func (a *App) publish(robotID, name string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	for sub := range a.subs {
		if a.canSee(sub.session, robotID) {
			select {
			case sub.events <- sseEvent{name: name, data: data}:
			default:
			}
		}
	}
}

func (a *App) publishRobot(r *robot) { a.publish(r.id, "robot", r.view()) }

// handleEvents streams SSE: "robot" (full view), "telemetry" {robot, m, ts}
// and "robot_event".
func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	session := a.session(w, r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")

	sub := &subscriber{session: session, events: make(chan sseEvent, 256)}
	a.mu.Lock()
	var first []sseEvent
	for _, x := range a.visible(session) {
		first = append(first, sseEvent{name: "robot", data: mustJSON(x.view())})
	}
	a.subs[sub] = struct{}{}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.subs, sub)
		a.mu.Unlock()
	}()

	send := func(ev sseEvent) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, ev := range first {
		if !send(ev) {
			return
		}
	}
	fmt.Fprint(w, ": ready\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-sub.events:
			if !send(ev) {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

/* -------------------------------- commands -------------------------------- */

// Sent many times a second while someone drives: not logged, not in the event list.
var quietCommands = map[string]bool{"car_drive": true, "ping": true, "look": true}

func (a *App) handleCommand(w http.ResponseWriter, r *http.Request) {
	session := a.session(w, r)
	id := r.PathValue("id")
	// Requiring JSON forces a CORS preflight for cross-site requests.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var cmd wire.RobotCommandBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&cmd); err != nil || cmd.Command == "" {
		http.Error(w, `body must be {"command": "..."}`, http.StatusBadRequest)
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	rob := a.robots[id]
	switch {
	case rob == nil || !a.canSee(session, id):
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
		return
	case serverOnly[cmd.Command] || !slices.Contains(rob.caps.Commands, cmd.Command):
		http.Error(w, "robot does not take command "+cmd.Command, http.StatusBadRequest)
		return
	case rob.conn == nil:
		http.Error(w, "robot is offline", http.StatusConflict)
		return
	}
	name, args, cm, limited := a.guardDrive(rob, cmd.Command, cmd.Args)
	if !rob.conn.command(name, args) {
		http.Error(w, "robot is not accepting commands", http.StatusServiceUnavailable)
		return
	}
	a.hubCommand(rob, browserSource(session), name, args, "")
	if !quietCommands[cmd.Command] {
		a.log.Info("command sent", "robot", id, "command", cmd.Command)
		a.addEvent(rob, eventView{Robot: id, Name: cmd.Command, Data: cmd.Args, Sent: true, TS: time.Now()})
	}
	if limited {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "safety_stop", "cm": math.Round(cm*10) / 10, "limit_cm": rob.safetyCm})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}

/* ---------------------------------- media --------------------------------- */

type mediaSub struct {
	robot   string
	session string
	video   bool
	out     chan []byte
}

// Browsers send the session cookie with the WebSocket handshake, so only
// pages from this server's own origin may open one.
var browserUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 16 << 10,
	CheckOrigin: func(r *http.Request) bool {
		u, err := url.Parse(r.Header.Get("Origin"))
		return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
	},
}

// handleMedia is a WebSocket carrying the robot's camera frames (binary
// wire.BinCameraJPEG messages) while ?video=1.
func (a *App) handleMedia(w http.ResponseWriter, r *http.Request) {
	session := a.session(w, r)
	id := r.PathValue("id")
	a.mu.Lock()
	ok := a.robots[id] != nil && a.canSee(session, id)
	a.mu.Unlock()
	if !ok {
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
		return
	}
	ws, err := browserUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	sub := &mediaSub{robot: id, session: session, video: r.URL.Query().Get("video") == "1", out: make(chan []byte, 4)}
	a.mu.Lock()
	a.media[sub] = struct{}{}
	if rob := a.robots[id]; rob != nil {
		a.updateStreams(rob)
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.media, sub)
		if rob := a.robots[id]; rob != nil {
			a.updateStreams(rob)
		}
		a.mu.Unlock()
	}()

	closed := make(chan struct{})
	go func() { // the browser sends nothing; reading notices when it goes away
		defer close(closed)
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-closed:
			return
		case msg := <-sub.out:
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := ws.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				return
			}
		case <-ping.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		}
	}
}

// handleSafety sets a car's safety stop limit: {"cm": 0 (off), 5, 10, 15, 20 or 30}.
func (a *App) handleSafety(w http.ResponseWriter, r *http.Request) {
	session := a.session(w, r)
	id := r.PathValue("id")
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	var body struct {
		Cm *int `json:"cm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil || body.Cm == nil || !slices.Contains(safetyChoicesCm, *body.Cm) {
		http.Error(w, fmt.Sprintf(`body must be {"cm": n} with n one of %v`, safetyChoicesCm), http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rob := a.robots[id]
	if rob == nil || !a.canSee(session, id) || !isCar(rob) {
		http.Error(w, "no car paired with this browser", http.StatusForbidden)
		return
	}
	rob.safetyCm = *body.Cm
	a.log.Info("safety limit", "robot", id, "cm", rob.safetyCm)
	a.addEvent(rob, eventView{Robot: id, Name: "safety_limit", Data: map[string]any{"cm": rob.safetyCm}, Sent: true, TS: time.Now()})
	a.setSafety(rob, rob.obstacleCm())
	a.publishRobot(rob)
	a.requestSave()
	writeJSON(w, http.StatusOK, map[string]int{"limit_cm": rob.safetyCm})
}

// handleSnapshot serves the latest full-resolution still the robot sent
// after a "snapshot" command.
func (a *App) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	session := a.session(w, r)
	id := r.PathValue("id")
	a.mu.Lock()
	rob := a.robots[id]
	ok := rob != nil && a.canSee(session, id)
	var jpeg []byte
	if ok {
		jpeg = rob.snapshot
	}
	a.mu.Unlock()
	switch {
	case !ok:
		http.Error(w, "robot not paired with this browser", http.StatusForbidden)
	case jpeg == nil:
		http.Error(w, "no snapshot yet", http.StatusNotFound)
	default:
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(jpeg)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
