# sbot

Embody Mode app server for a Stack-chan plus linked devices (first: a TPBot car via `../tpbot-ble`). See [readme.md](readme.md).

- Workspace notes for the whole Stack-chan effort are in `../stackchan-mj/AGENTS.md`. Read them first.
- Go only: the standard library plus gorilla/websocket. The protocol comes from `github.com/mj41/stackchan-server/wire` (`replace` to `../stackchan-server`).
- The car is optional: a Stack-chan without a car must work as before. Car support keys off the `car_*` commands, never the model name, so the later "Stack-chan hosts the car" step needs no change here.
- The car capability (command and telemetry names) is shared with `tpbot-ble/cmd/tpbot-bridge`: change both readmes together.
- Primary data only: show raw values; derived reactions (obstacle, line following) are app logic and must be explicit.
- Before committing, run `gofmt -l .`, `go vet ./...` and `go test -race ./...`.
- LAN dev run: `../stackchan-mj/sbot-bg.sh start|restart|log` (port 8780, page from disk).
- Don't open camera frames or snapshots unless the user asks (people at home).
- The event hub (`internal/hub`, NATS JetStream embedded) is not in the path between a browser and a device: publishing never blocks a command. Keep it that way.
- Loops (`internal/loops`) only request commands; `app.loopCommand` applies grants and the normal checks. A new loop needs a `-loop-grant` and replay → shadow → live before it is used.
- The hub token and session ids are credentials: never log or publish them (browsers appear as `browser:<hash>`).
