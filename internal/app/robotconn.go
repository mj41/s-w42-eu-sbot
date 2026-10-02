package app

import (
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mj41/stackchan-server/wire"
)

// Liveness timing, as in stackchan-server.
const (
	pingPeriod      = 5 * time.Second
	pongWait        = 60 * time.Second
	writeWait       = 10 * time.Second
	registerTimeout = 10 * time.Second
	maxMessageBytes = 256 << 10
)

var robotIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Robots authenticate with a bearer token, not cookies.
	CheckOrigin: func(r *http.Request) bool { return true },
}

type outMsg struct {
	binary bool
	data   []byte
}

// robotConn is one robot's socket; writeLoop owns all writes.
type robotConn struct {
	id        string
	ws        *websocket.Conn
	send      chan outMsg
	closeOnce sync.Once
	done      chan struct{}
}

func (c *robotConn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.ws.Close()
	})
}

func (c *robotConn) queue(m outMsg) bool {
	select {
	case <-c.done:
		return false
	case c.send <- m:
		return true
	default:
		return false
	}
}

func (c *robotConn) frame(kind string, body any) bool {
	f, err := wire.Marshal(kind, wire.Meta{WorkerID: c.id}, body)
	return err == nil && c.queue(outMsg{data: f})
}

func (c *robotConn) command(name string, args map[string]any) bool {
	return c.frame(wire.KindRobotCommand, wire.RobotCommandBody{Command: name, Args: args})
}

func (a *App) tokenOK(token string) bool {
	return a.cfg.RobotToken != "" && token == a.cfg.RobotToken
}

func (a *App) handleRobotConnect(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !a.tokenOK(token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.Header.Get(wire.WorkerIDHeader)
	if !robotIDPattern.MatchString(id) {
		http.Error(w, "missing or invalid "+wire.WorkerIDHeader, http.StatusBadRequest)
		return
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		a.log.Warn("robot upgrade failed", "robot", id, "err", err)
		return
	}
	ws.SetReadLimit(maxMessageBytes)
	c := &robotConn{id: id, ws: ws, send: make(chan outMsg, 64), done: make(chan struct{})}
	defer c.close()

	reg, reason := readRegister(ws, id)
	if reason != "" {
		a.log.Warn("robot rejected", "robot", id, "reason", reason)
		if f, err := wire.Marshal(wire.KindRejected, wire.Meta{}, wire.RejectedBody{Reason: reason}); err == nil {
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			ws.WriteMessage(websocket.TextMessage, f)
		}
		return
	}
	a.log.Info("robot connected", "robot", id, "model", reg.Capabilities.Model, "firmware", reg.Capabilities.Firmware, "with", reg.Labels["with"])
	defer a.log.Info("robot disconnected", "robot", id)

	a.attach(c, reg)
	defer a.detach(c)
	go a.writeLoop(c)
	a.readLoop(c)
}

func readRegister(ws *websocket.Conn, id string) (wire.RegisterBody, string) {
	var reg wire.RegisterBody
	ws.SetReadDeadline(time.Now().Add(registerTimeout))
	_, data, err := ws.ReadMessage()
	if err != nil {
		return reg, "no Register frame"
	}
	frames, err := wire.Parse(data)
	if err != nil || len(frames) == 0 || frames[0].Kind != wire.KindRegister {
		return reg, "first frame must be Register"
	}
	f := frames[0]
	if f.Meta.WorkerID != "" && f.Meta.WorkerID != id {
		return reg, "meta.worker_id does not match " + wire.WorkerIDHeader
	}
	if err := f.Decode(&reg); err != nil {
		return reg, "invalid Register body"
	}
	if reg.Class != wire.ClassRobot {
		return reg, "unknown worker class"
	}
	if w := reg.Labels["with"]; w != "" && (!robotIDPattern.MatchString(w) || w == id) {
		return reg, "invalid label with"
	}
	return reg, ""
}

func (a *App) attach(c *robotConn, reg wire.RegisterBody) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.robotFor(c.id)
	old := r.conn
	r.conn = c
	r.caps = reg.Capabilities
	r.with = reg.Labels["with"]
	r.lastSeen = time.Now()
	r.cameraOn = false
	if old != nil {
		old.close()
	}
	c.frame(wire.KindAccepted, nil)
	// Pictures from the server it came from (e.g. the pet's game) stay on the
	// robot across servers: start from the plain face.
	if slices.Contains(r.caps.Commands, "sprite_clear") {
		c.command("sprite_clear", nil)
	}
	if slices.Contains(r.caps.Commands, "face") {
		c.command("face", nil)
	}
	c.frame(wire.KindPairCode, a.issueCode(c.id))
	if n := a.viewers(c.id); n > 0 {
		c.frame(wire.KindPaired, wire.PairedBody{Viewers: n, Reconnect: true})
	}
	a.updateStreams(r)
	a.requestSave()
	a.publishRobot(r)
	a.hubEvent(r, "server", "online", map[string]any{"model": r.caps.Model, "firmware": r.caps.Firmware})
}

func (a *App) detach(c *robotConn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.robots[c.id]
	if r == nil || r.conn != c {
		return
	}
	r.conn = nil
	r.cameraOn = false
	a.hubEvent(r, "server", "offline", nil)
	for code, pc := range a.codes {
		if pc.robotID == c.id {
			delete(a.codes, code)
		}
	}
	a.publishRobot(r)
}

func (a *App) writeLoop(c *robotConn) {
	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()
	rotate := time.NewTicker(a.cfg.PairTTL)
	defer rotate.Stop()
	for {
		select {
		case <-c.done:
			return
		case m := <-c.send:
			kind := websocket.TextMessage
			if m.binary {
				kind = websocket.BinaryMessage
			}
			c.ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.ws.WriteMessage(kind, m.data); err != nil {
				c.close()
				return
			}
		case <-ping.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				c.close()
				return
			}
		case <-rotate.C:
			a.mu.Lock()
			body := a.issueCode(c.id)
			a.mu.Unlock()
			c.frame(wire.KindPairCode, body)
		}
	}
}

func (a *App) readLoop(c *robotConn) {
	resetDeadline := func() { c.ws.SetReadDeadline(time.Now().Add(pongWait)) }
	resetDeadline()
	c.ws.SetPongHandler(func(string) error { resetDeadline(); return nil })
	c.ws.SetPingHandler(func(data string) error {
		resetDeadline()
		return c.ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeWait))
	})
	for {
		kind, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		resetDeadline()
		if kind == websocket.BinaryMessage {
			a.relayMedia(c.id, data)
			continue
		}
		frames, err := wire.Parse(data)
		if err != nil {
			a.log.Warn("bad frame from robot", "robot", c.id, "err", err)
			continue
		}
		for _, f := range frames {
			a.handleRobotFrame(c, f)
		}
	}
}

func (a *App) handleRobotFrame(c *robotConn, f wire.Frame) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.robots[c.id]
	if r == nil || r.conn != c {
		return
	}
	r.lastSeen = time.Now()
	switch f.Kind {
	case wire.KindRobotTelemetry:
		var body wire.RobotTelemetryBody
		if err := f.Decode(&body); err != nil {
			return
		}
		for k, v := range body.Measurements {
			r.telemetry[k] = v
		}
		r.telemetryAt = time.Now()
		a.publish(r.id, "telemetry", map[string]any{"robot": r.id, "m": body.Measurements, "ts": r.telemetryAt})
		a.hubTelemetry(r, body.Measurements)
		if _, ok := body.Measurements["car_echo_us"]; ok {
			a.checkSafety(r)
		}
	case wire.KindRobotEvent:
		var body wire.RobotEventBody
		if err := f.Decode(&body); err != nil || body.Name == "" {
			return
		}
		a.log.Info("robot event", "robot", r.id, "name", body.Name)
		a.hubEvent(r, "device:"+r.id, body.Name, body.Data)
		if body.Name == "servers" {
			r.servers = body.Data
			a.publishRobot(r)
			return
		}
		a.addEvent(r, eventView{Robot: r.id, Name: body.Name, Data: body.Data, TS: time.Now()})
	}
}

/* ---------------------------------- media --------------------------------- */

// updateStreams turns the robot's camera on while a browser watches it, and
// off otherwise. Call with a.mu held.
func (a *App) updateStreams(r *robot) {
	if r.conn == nil || !slices.Contains(r.caps.Commands, "camera") {
		return
	}
	want := false
	for sub := range a.media {
		if sub.robot == r.id && sub.video {
			want = true
			break
		}
	}
	if want != r.cameraOn {
		r.cameraOn = want
		r.conn.command("camera", map[string]any{"on": want})
	}
}

func (a *App) relayMedia(robotID string, msg []byte) {
	if len(msg) < 2 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if msg[0] == wire.BinSnapshot {
		if r := a.robots[robotID]; r != nil {
			r.snapshot, r.snapshotAt = msg[1:], time.Now()
			a.publish(robotID, "snapshot", map[string]any{"robot": robotID, "bytes": len(msg) - 1, "ts": r.snapshotAt})
		}
		return
	}
	if msg[0] != wire.BinCameraJPEG {
		return
	}
	for sub := range a.media {
		if sub.robot == robotID && sub.video {
			select {
			case sub.out <- msg:
			default: // a slow browser skips frames rather than delaying the next ones
			}
		}
	}
}
