// Package hub is the home's event hub (home-w42-eu architecture §6): a log-based
// message hub with Kafka-like streams, here NATS JetStream embedded in sbot.
//
// Subjects:
//
//	telemetry.<device>               device measurements (stored downsampled, see app)
//	live.telemetry.<device>          every measurement, not stored (live consumers)
//	events.<device>.<name>           device events
//	commands.<device>.<command>      commands sent to devices, with their source
//	decisions.<maker>.<device>       what loops and controllers decided, and why
//	requests.commands.<device>       request/reply: a loop asks the web/API server to
//	                                 send a command (the policy decides; not stored)
//
// Device ids may contain characters NATS uses in subjects; Token makes them safe.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Event is the envelope of everything in the hub.
type Event struct {
	ID     string         `json:"id"` // sortable: time first
	TS     time.Time      `json:"ts"`
	Source string         `json:"source"` // device:<id>, browser:<x>, loop:<name>, server
	Kind   string         `json:"kind"`   // telemetry, event, command, decision
	Device string         `json:"device,omitempty"`
	Name   string         `json:"name,omitempty"`
	Data   map[string]any `json:"data,omitempty"`
	Cause  string         `json:"cause,omitempty"` // id of the event that caused this one
}

// NewID returns a sortable unique id: nanoseconds since 1970, then random bits.
func NewID(t time.Time) string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%016x%s", t.UnixNano(), hex.EncodeToString(b))
}

// Token makes a device id or a name safe as one subject token.
func Token(s string) string {
	r := strings.NewReplacer(".", "_", "*", "_", ">", "_", " ", "_")
	if s = r.Replace(s); s == "" {
		return "_"
	}
	return s
}

// Streams: 7 days hot (home-w42-eu principle on retention), size caps for a small machine.
var streams = []jetstream.StreamConfig{
	{Name: "TELEMETRY", Subjects: []string{"telemetry.>"}, MaxAge: 7 * 24 * time.Hour, MaxBytes: 512 << 20},
	{Name: "EVENTS", Subjects: []string{"events.>"}, MaxAge: 7 * 24 * time.Hour, MaxBytes: 128 << 20},
	{Name: "COMMANDS", Subjects: []string{"commands.>"}, MaxAge: 7 * 24 * time.Hour, MaxBytes: 128 << 20},
	{Name: "DECISIONS", Subjects: []string{"decisions.>"}, MaxAge: 7 * 24 * time.Hour, MaxBytes: 128 << 20},
}

// Hub is a connection to the hub, with the embedded server when this process runs it.
type Hub struct {
	NC  *nats.Conn
	JS  jetstream.JetStream
	srv *server.Server
}

type Config struct {
	StoreDir string // JetStream files
	Listen   string // host:port for other processes (the controller server); "" = in-process only
	Token    string // required from other processes
}

// Start runs the embedded server and connects to it in-process.
func Start(cfg Config) (*Hub, error) {
	opts := &server.Options{
		ServerName: "sbot-hub",
		JetStream:  true,
		StoreDir:   cfg.StoreDir,
		NoSigs:     true,
		NoLog:      true,
		DontListen: cfg.Listen == "",
	}
	if cfg.Listen != "" {
		host, port, err := splitHostPort(cfg.Listen)
		if err != nil {
			return nil, err
		}
		if cfg.Token == "" {
			return nil, errors.New("hub: a token is required when it listens")
		}
		opts.Host, opts.Port, opts.Authorization = host, port, cfg.Token
	}
	srv, err := server.NewServer(opts)
	if err != nil {
		return nil, err
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		return nil, errors.New("hub: server did not start")
	}
	nc, err := nats.Connect("", nats.InProcessServer(srv), nats.Token(cfg.Token), nats.Name("sbot"))
	if err != nil {
		srv.Shutdown()
		return nil, err
	}
	h, err := wrap(nc)
	if err != nil {
		nc.Close()
		srv.Shutdown()
		return nil, err
	}
	h.srv = srv
	if err := h.ensureStreams(); err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

// Connect joins a hub run by another process (the controller server joins sbot's).
func Connect(url, token, name string) (*Hub, error) {
	nc, err := nats.Connect(url, nats.Token(token), nats.Name(name), nats.MaxReconnects(-1))
	if err != nil {
		return nil, err
	}
	return wrap(nc)
}

func wrap(nc *nats.Conn) (*Hub, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, err
	}
	return &Hub{NC: nc, JS: js}, nil
}

func (h *Hub) ensureStreams() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, sc := range streams {
		sc.Storage = jetstream.FileStorage
		sc.Discard = jetstream.DiscardOld
		if _, err := h.JS.CreateOrUpdateStream(ctx, sc); err != nil {
			return fmt.Errorf("hub: stream %s: %w", sc.Name, err)
		}
	}
	return nil
}

func (h *Hub) Close() {
	h.NC.Drain()
	if h.srv != nil {
		h.srv.Shutdown()
		h.srv.WaitForShutdown()
	}
}

// Publish sends an event on a subject. It does not wait: the hub is not in the
// path between a browser and a device.
func (h *Hub) Publish(subject string, ev Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return h.NC.Publish(subject, b)
}

// Follow delivers stored events on the subjects, oldest first: from since (replay)
// or only new ones (since zero). It returns when ctx ends, or, with untilNow, when it
// has caught up with what was stored when it started.
func (h *Hub) Follow(ctx context.Context, subjects []string, since time.Time, untilNow bool, f func(Event)) error {
	stream, err := h.streamFor(ctx, subjects)
	if err != nil {
		return err
	}
	cfg := jetstream.OrderedConsumerConfig{FilterSubjects: subjects, DeliverPolicy: jetstream.DeliverNewPolicy}
	if !since.IsZero() {
		cfg.DeliverPolicy, cfg.OptStartTime = jetstream.DeliverByStartTimePolicy, &since
	}
	cons, err := stream.OrderedConsumer(ctx, cfg)
	if err != nil {
		return err
	}
	if untilNow {
		info, err := cons.Info(ctx)
		if err != nil {
			return err
		}
		if info.NumPending == 0 {
			return nil
		}
	}
	it, err := cons.Messages()
	if err != nil {
		return err
	}
	defer it.Stop()
	go func() { <-ctx.Done(); it.Stop() }()
	for {
		msg, err := it.Next()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				return nil
			}
			return err
		}
		var ev Event
		if json.Unmarshal(msg.Data(), &ev) == nil {
			f(ev)
		}
		if untilNow {
			if md, err := msg.Metadata(); err == nil && md.NumPending == 0 {
				return nil
			}
		}
	}
}

// streamFor finds the stream that stores the first subject.
func (h *Hub) streamFor(ctx context.Context, subjects []string) (jetstream.Stream, error) {
	if len(subjects) == 0 {
		return nil, errors.New("hub: no subjects")
	}
	name, err := h.JS.StreamNameBySubject(ctx, subjects[0])
	if err != nil {
		return nil, fmt.Errorf("hub: no stream stores %s: %w", subjects[0], err)
	}
	return h.JS.Stream(ctx, name)
}

func splitHostPort(s string) (string, int, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", 0, fmt.Errorf("hub: listen %q: want host:port", s)
	}
	var port int
	if _, err := fmt.Sscanf(s[i+1:], "%d", &port); err != nil {
		return "", 0, fmt.Errorf("hub: listen %q: %w", s, err)
	}
	return s[:i], port, nil
}
