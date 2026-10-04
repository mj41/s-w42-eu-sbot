// Package app is sbot: an Embody Mode app server that manages a Stackchan
// and the devices that work with it, starting with a TPBot car.
//
// Every device is a "robot" worker (s-w42-eu-raw wire protocol). A worker
// that lists car_* commands has a car. A worker with the Register label
// "with" = <robot id> belongs to that robot: browsers paired with the robot
// also see and drive it.
package app

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mj41/s-w42-eu-sbot/internal/hub"
	"github.com/mj41/s-w42-eu-raw/wire"
)

type Config struct {
	RobotToken string
	PublicURL  string // base URL put into pairing QR codes
	PairTTL    time.Duration
	UIDir      string              // development: serve the UI from disk
	StateFile  string              // "" keeps no state
	Hub        *hub.Hub            // the event hub; nil = none (hubio.go)
	LoopGrants map[string][]string // loop name -> commands it may request; none by default
	Log        *slog.Logger
}

type App struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	robots   map[string]*robot
	codes    map[string]pairCode
	sessions map[string]map[string]bool // session -> robot ids paired by code
	subs     map[*subscriber]struct{}
	media    map[*mediaSub]struct{}
	saveNow  chan struct{}
	saveMu   sync.Mutex
}

type robot struct {
	id           string
	caps         wire.RobotCapabilities
	with         string     // the robot this one belongs to, from the Register label
	conn         *robotConn // nil while offline
	lastSeen     time.Time
	telemetry    map[string]float64
	telemetryAt  time.Time
	events       []eventView    // newest last
	cameraOn     bool           // what we last asked the robot
	servers      map[string]any // data of the robot's latest "servers" event
	snapshot     []byte         // latest full-resolution still (JPEG), in memory only
	snapshotAt   time.Time
	safetyCm     int    // safety stop limit for a car, 0 = off (safety.go)
	name, room   string // the home model's words for this device (homemodel.go)
	hub          hubState
	safetyActive bool // an obstacle is closer than the limit now
	lastEchoCm   float64
	lastEchoAt   time.Time // the last reading with an echo
}

type pairCode struct {
	robotID string
	expires time.Time
}

const keepEvents = 20

// Commands the server sends on its own; browsers may not.
var serverOnly = map[string]bool{"camera": true, "mic": true, "imu_stream": true, "touch_stream": true, "light_stream": true}

func New(cfg Config) *App {
	if cfg.PairTTL <= 0 {
		cfg.PairTTL = 5 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	a := &App{
		cfg:      cfg,
		log:      cfg.Log,
		robots:   map[string]*robot{},
		codes:    map[string]pairCode{},
		sessions: map[string]map[string]bool{},
		subs:     map[*subscriber]struct{}{},
		media:    map[*mediaSub]struct{}{},
		saveNow:  make(chan struct{}, 1),
	}
	if cfg.StateFile != "" {
		if err := a.loadState(); err != nil {
			a.log.Warn("state not loaded, starting empty", "file", cfg.StateFile, "err", err)
		}
	}
	return a
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+wire.ConnectPath, a.handleRobotConnect)
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /pair", a.handlePair)
	mux.HandleFunc("GET /api/robots", a.handleListRobots)
	mux.HandleFunc("GET /api/events", a.handleEvents)
	mux.HandleFunc("POST /api/robots/{id}/command", a.handleCommand)
	mux.HandleFunc("GET /api/robots/{id}/media", a.handleMedia)
	mux.HandleFunc("GET /api/robots/{id}/snapshot", a.handleSnapshot)
	mux.HandleFunc("POST /api/robots/{id}/safety", a.handleSafety)
	mux.HandleFunc("POST /api/robots/{id}/name", a.handleName)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

func (a *App) robotFor(id string) *robot {
	r := a.robots[id]
	if r == nil {
		r = &robot{id: id, telemetry: map[string]float64{}, safetyCm: defaultSafetyCm}
		a.robots[id] = r
	}
	return r
}

// canSee: the session paired with the robot, or with the robot it belongs to.
// Call with a.mu held.
func (a *App) canSee(session, id string) bool {
	paired := a.sessions[session]
	if paired[id] {
		return true
	}
	r := a.robots[id]
	return r != nil && r.with != "" && paired[r.with]
}

func (a *App) visible(session string) []*robot {
	var out []*robot
	for id, r := range a.robots {
		if a.canSee(session, id) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(x, y *robot) int { return strings.Compare(x.id, y.id) })
	return out
}

/* --------------------------------- views ---------------------------------- */

type robotView struct {
	ID          string             `json:"id"`
	Online      bool               `json:"online"`
	Model       string             `json:"model,omitempty"`
	Firmware    string             `json:"firmware,omitempty"`
	With        string             `json:"with,omitempty"`
	Car         bool               `json:"car"`
	Camera      bool               `json:"camera"`
	Commands    []string           `json:"commands"`
	Telemetry   map[string]float64 `json:"telemetry"`
	TelemetryAt *time.Time         `json:"telemetry_at,omitempty"`
	LastSeen    time.Time          `json:"last_seen"`
	Events      []eventView        `json:"events"`
	Servers     map[string]any     `json:"servers,omitempty"` // the robot's server list: {list, current, default}
	Safety      *safetyView        `json:"safety,omitempty"`  // cars only
	Name        string             `json:"name"`              // what people call it; the id when unnamed
	Room        string             `json:"room,omitempty"`
}

type safetyView struct {
	LimitCm int   `json:"limit_cm"` // 0 = off
	Active  bool  `json:"active"`   // forward driving is blocked now
	Choices []int `json:"choices"`
}

type eventView struct {
	Robot string         `json:"robot"`
	Name  string         `json:"name"`
	Data  map[string]any `json:"data,omitempty"`
	Sent  bool           `json:"sent,omitempty"` // a command a browser sent
	TS    time.Time      `json:"ts"`
}

func (r *robot) view() robotView {
	v := robotView{
		ID: r.id, Online: r.conn != nil, Model: r.caps.Model, Firmware: r.caps.Firmware, With: r.with,
		Name: r.displayName(), Room: r.room,
		Car:      slices.Contains(r.caps.Commands, "car_drive"),
		Camera:   slices.Contains(r.caps.Commands, "camera"),
		Commands: r.caps.Commands, Telemetry: r.telemetry, LastSeen: r.lastSeen,
		Events: slices.Clone(r.events), Servers: r.servers,
	}
	if v.Car {
		v.Safety = &safetyView{LimitCm: r.safetyCm, Active: r.safetyActive, Choices: safetyChoicesCm}
	}
	if v.Commands == nil {
		v.Commands = []string{}
	}
	if v.Events == nil {
		v.Events = []eventView{}
	}
	if !r.telemetryAt.IsZero() {
		t := r.telemetryAt
		v.TelemetryAt = &t
	}
	return v
}

func (a *App) addEvent(r *robot, ev eventView) {
	r.events = append(r.events, ev)
	if len(r.events) > keepEvents {
		r.events = r.events[len(r.events)-keepEvents:]
	}
	a.publish(r.id, "robot_event", ev)
}

/* -------------------------------- pairing --------------------------------- */

// Unambiguous characters only (no 0/O, 1/I), as in s-w42-eu-raw.
const codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

func newCode() string {
	b := make([]byte, 8)
	rand.Read(b)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b)
}

// issueCode replaces the robot's pairing code. Call with a.mu held.
func (a *App) issueCode(robotID string) wire.PairCodeBody {
	for c, pc := range a.codes {
		if pc.robotID == robotID {
			delete(a.codes, c)
		}
	}
	code := newCode()
	a.codes[code] = pairCode{robotID: robotID, expires: time.Now().Add(a.cfg.PairTTL)}
	return wire.PairCodeBody{Code: code, URL: a.cfg.PublicURL + "/pair?code=" + code, ExpiresInS: int(a.cfg.PairTTL / time.Second)}
}

// viewers counts the sessions paired with the robot. Call with a.mu held.
func (a *App) viewers(robotID string) int {
	n := 0
	for _, p := range a.sessions {
		if p[robotID] {
			n++
		}
	}
	return n
}

const sessionCookie = "sbot_session"

func (a *App) session(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && validSessionID(c.Value) {
		return c.Value
	}
	b := make([]byte, 32)
	rand.Read(b)
	id := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/",
		MaxAge:   int((365 * 24 * time.Hour) / time.Second),
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || strings.HasPrefix(a.cfg.PublicURL, "https://"),
	})
	return id
}

func validSessionID(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}
