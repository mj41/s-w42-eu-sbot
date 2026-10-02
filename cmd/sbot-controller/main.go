// Command sbot-controller is the controller server of the home node: it runs loops
// as consumers of sbot's event hub (home-w42-eu architecture §7).
//
//	sbot-controller -mode replay -since 24h   what the loops would have done
//	sbot-controller -mode shadow              decisions only, nothing sent
//	sbot-controller -mode live                commands through sbot's policy
//	sbot-controller -mode stats               what the hub's streams hold
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mj41/sbot/internal/hub"
	"github.com/mj41/sbot/internal/loops"
)

var known = map[string]func() loops.Loop{
	"frown": func() loops.Loop { return &loops.Frown{} },
}

func main() {
	home, _ := os.UserHomeDir()
	var (
		url       = flag.String("hub", "nats://127.0.0.1:4222", "sbot's event hub")
		tokenFile = flag.String("hub-token-file", filepath.Join(home, ".config/sbot/hub-token"), "the hub's token (written by sbot)")
		mode      = flag.String("mode", "shadow", "replay, shadow, live, or stats")
		since     = flag.Duration("since", 24*time.Hour, "replay: how far back to start")
		names     = flag.String("loops", "frown", "comma-separated loops to run")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	m := loops.Mode(*mode)
	if m != loops.Replay && m != loops.Shadow && m != loops.Live && *mode != "stats" {
		log.Error("-mode must be replay, shadow, live or stats")
		os.Exit(2)
	}
	tok, err := os.ReadFile(*tokenFile)
	if err != nil {
		log.Error("hub token", "err", err)
		os.Exit(1)
	}
	h, err := hub.Connect(*url, strings.TrimSpace(string(tok)), "sbot-controller")
	if err != nil {
		log.Error("hub", "err", err)
		os.Exit(1)
	}
	defer h.Close()
	if *mode == "stats" {
		stats(h)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := &loops.Runner{Hub: h, Mode: m, Log: log}
	start := time.Now().Add(-*since)
	var wg sync.WaitGroup
	for _, n := range strings.Split(*names, ",") {
		mk, ok := known[strings.TrimSpace(n)]
		if !ok {
			log.Error("unknown loop", "loop", n)
			os.Exit(2)
		}
		l := mk()
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info("loop started", "loop", l.Name(), "mode", m)
			actions, err := r.Run(ctx, l, start)
			if err != nil {
				log.Error("loop stopped", "loop", l.Name(), "err", err)
			}
			if m == loops.Replay {
				fmt.Print(loops.Report(l.Name(), start, actions))
			}
		}()
	}
	wg.Wait()
}

func stats(h *hub.Hub) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name := range h.JS.StreamNames(ctx).Name() {
		s, err := h.JS.Stream(ctx, name)
		if err != nil {
			continue
		}
		i, err := s.Info(ctx)
		if err != nil {
			continue
		}
		age := "-"
		if !i.State.FirstTime.IsZero() {
			age = time.Since(i.State.FirstTime).Round(time.Second).String()
		}
		fmt.Printf("%-10s %7d messages %10d bytes  %3d subjects  oldest %s ago\n", name, i.State.Msgs, i.State.Bytes, i.State.NumSubjects, age)
	}
}
