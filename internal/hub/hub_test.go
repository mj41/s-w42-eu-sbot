package hub

import (
	"context"
	"testing"
	"time"
)

func TestPublishAndReplay(t *testing.T) {
	h, err := Start(Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	start := time.Now().Add(-time.Second)
	for i, name := range []string{"safety", "safety", "other"} {
		ev := Event{ID: NewID(time.Now()), TS: time.Now(), Source: "server", Kind: "decision", Device: "tpbot.2125", Name: name, Data: map[string]any{"n": i}}
		subject := "decisions." + name + "." + Token(ev.Device)
		if err := h.Publish(subject, ev); err != nil {
			t.Fatal(err)
		}
	}
	h.NC.Flush()
	time.Sleep(100 * time.Millisecond) // the stream stores asynchronously

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got []Event
	if err := h.Follow(ctx, []string{"decisions.safety.>"}, start, true, func(ev Event) { got = append(got, ev) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Data["n"] != 0.0 || got[1].Data["n"] != 1.0 || got[0].Device != "tpbot.2125" {
		t.Fatalf("replay got %+v", got)
	}
	if got[0].ID >= got[1].ID {
		t.Fatalf("ids not sortable: %s %s", got[0].ID, got[1].ID)
	}
}

func TestToken(t *testing.T) {
	for in, want := range map[string]string{"stackchan-0a1b2c3d4e50": "stackchan-0a1b2c3d4e50", "a.b": "a_b", "x>y*z": "x_y_z", "": "_"} {
		if got := Token(in); got != want {
			t.Errorf("Token(%q) = %q, want %q", in, got, want)
		}
	}
}
