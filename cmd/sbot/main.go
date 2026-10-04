// Command sbot is an Embody Mode app server for a Stackchan and the devices
// that work with it (first: a TPBot car through tpbot-bridge).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mj41/s-w42-eu-sbot/internal/app"
	"github.com/mj41/s-w42-eu-sbot/internal/hub"
)

func main() {
	var (
		listen    = flag.String("listen", ":8780", "HTTP listen address for robots and browsers")
		publicURL = flag.String("public-url", "", "base URL browsers use to reach sbot (default: http://<LAN IP>:<port>)")
		tokenFile = flag.String("token-file", configFile("stackchan-server", "robot-token"), "file with the robot bearer token (the same as s-w42-eu-raw's, so it can offer sbot)")
		pairTTL   = flag.Duration("pair-ttl", 5*time.Minute, "lifetime of a pairing code")
		stateFile = flag.String("state-file", stateFile(), "JSON file that keeps pairings and robots across restarts (\"\" disables)")
		uiDir     = flag.String("ui-dir", "", "development: serve index.html from this directory on every request (e.g. internal/app/ui)")
		hubDir    = flag.String("hub-dir", stateDir("hub"), "event hub (NATS JetStream) files (\"\" runs without the hub)")
		hubListen = flag.String("hub-listen", "127.0.0.1:4222", "address for the controller server to reach the hub (\"\" = in-process only)")
		hubToken  = flag.String("hub-token-file", configFile("sbot", "hub-token"), "token the controller server needs; generated if missing")
		grants    = grantFlags{}
	)
	flag.Var(grants, "loop-grant", "loop=command[,command…]: commands a loop may request (repeatable; none by default)")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	b, err := os.ReadFile(*tokenFile)
	token := strings.TrimSpace(string(b))
	if err != nil || token == "" {
		log.Error("robot token", "file", *tokenFile, "err", err)
		os.Exit(1)
	}
	if *publicURL == "" {
		host, port, err := net.SplitHostPort(*listen)
		if err != nil {
			log.Error("parse -listen", "err", err)
			os.Exit(1)
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = lanIP()
		}
		*publicURL = "http://" + net.JoinHostPort(host, port)
	}

	var h *hub.Hub
	if *hubDir != "" {
		tok, err := loadOrCreateToken(*hubToken)
		if err != nil {
			log.Error("hub token", "file", *hubToken, "err", err)
			os.Exit(1)
		}
		if h, err = hub.Start(hub.Config{StoreDir: *hubDir, Listen: *hubListen, Token: tok}); err != nil {
			log.Error("event hub", "err", err)
			os.Exit(1)
		}
		defer h.Close()
		log.Info("event hub", "dir", *hubDir, "listen", *hubListen, "loop_grants", map[string][]string(grants))
	}

	a := app.New(app.Config{
		Hub:        h,
		LoopGrants: grants,
		RobotToken: token,
		PublicURL:  strings.TrimRight(*publicURL, "/"),
		PairTTL:    *pairTTL,
		UIDir:      *uiDir,
		StateFile:  *stateFile,
		Log:        log,
	})
	srv := &http.Server{Addr: *listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.RunStateSaver(ctx)
	if h != nil {
		if _, err := a.ServeLoopRequests(); err != nil {
			log.Error("loop requests", "err", err)
			os.Exit(1)
		}
	}
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()

	log.Info("sbot listening", "listen", *listen, "public_url", *publicURL, "state_file", *stateFile)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
	if err := a.SaveState(); err != nil {
		fmt.Fprintln(os.Stderr, "state not saved:", err)
	}
}

// lanIP returns the address of the interface used for outbound traffic.
// Dialing UDP sends no packets; it only picks a route.
func lanIP() string {
	if c, err := net.Dial("udp4", "192.0.2.1:80"); err == nil {
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).IP.String()
	}
	return "127.0.0.1"
}

func configFile(app, name string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return name
	}
	return filepath.Join(dir, app, name)
}

func stateFile() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "sbot", "state.json")
}

func stateDir(name string) string {
	f := stateFile()
	if f == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(f), name)
}

// loadOrCreateToken reads a token file, or writes a new random token (mode 0600).
func loadOrCreateToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return tok, os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

// grantFlags collects -loop-grant loop=cmd,cmd values.
type grantFlags map[string][]string

func (g grantFlags) String() string { return fmt.Sprint(map[string][]string(g)) }

func (g grantFlags) Set(v string) error {
	loop, cmds, ok := strings.Cut(v, "=")
	if !ok || loop == "" || cmds == "" {
		return fmt.Errorf("want loop=command[,command…]")
	}
	for _, c := range strings.Split(cmds, ",") {
		if c = strings.TrimSpace(c); c != "" {
			g[loop] = append(g[loop], c)
		}
	}
	return nil
}
