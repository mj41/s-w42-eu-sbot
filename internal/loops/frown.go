package loops

import "github.com/mj41/s-w42-eu-sbot/internal/hub"

// Frown: the robot looks sad while its car is blocked by an obstacle, and neutral
// again when it is clear (home-w42-eu use case 3). It reads the safety controller's
// decisions, which name the device with a face ("face").
type Frown struct {
	sad map[string]bool // face device -> currently sad because of a car
}

func (f *Frown) Name() string       { return "frown" }
func (f *Frown) Subjects() []string { return []string{"decisions.safety.>"} }

func (f *Frown) Handle(ev hub.Event, out Out) {
	if ev.Name != "safety" {
		return
	}
	face, _ := ev.Data["face"].(string)
	active, _ := ev.Data["active"].(bool)
	if face == "" {
		return
	}
	if f.sad == nil {
		f.sad = map[string]bool{}
	}
	if f.sad[face] == active {
		return
	}
	f.sad[face] = active
	emotion := "neutral"
	if active {
		emotion = "sad"
	}
	out.Command(face, "emotion", map[string]any{"name": emotion}, ev.ID)
	out.Decide(face, "frown", map[string]any{"active": active, "car": ev.Device, "cm": ev.Data["cm"]}, ev.ID)
}
