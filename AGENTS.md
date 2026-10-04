# sbot

Embody Mode app server for a Stackchan plus linked devices (first: a TPBot car via `../tpbot-ble`). See [readme.md](readme.md).

- Workspace notes for the whole Stackchan effort are in `../stackchan-mj/AGENTS.md`. Read them first.
- Go only: the standard library plus gorilla/websocket and NATS (embedded JetStream). The protocol comes from `github.com/mj41/stackchan-server/wire` (the version pinned in `go.mod`; an uncommitted `go.work` tries both together).
- The car is optional: a Stackchan without a car must work as before. Car support keys off the `car_*` commands, never the model name, so a car hosted by Stackchan or by `tpbot-bridge` looks the same.
- The car capability table (command and telemetry names) lives only in this readme (wire-protocol.md §8 points here). Change it together with `tpbot-ble/cmd/tpbot-bridge` and the firmware's car code.
- Primary data only: show raw values; derived reactions (obstacle, line following) are app logic and must be explicit.
- Before committing, run `gofmt -l .`, `go vet ./...` and `go test -race ./...`.
- LAN dev run: `../stackchan-mj/sbot-bg.sh start|restart|log` (port 8780, page from disk).
- Don't open camera frames or snapshots unless the user asks (people at home).
- The event hub (`internal/hub`, NATS JetStream embedded) is not in the path between a browser and a device: publishing never blocks a command. Keep it that way.
- Loops (`internal/loops`) only request commands; `app.loopCommand` applies grants and the normal checks. A new loop needs a `-loop-grant` and replay → shadow → live before it is used.
- The hub token and session ids are credentials: never log or publish them (browsers appear as `browser:<hash>`).
