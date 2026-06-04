# eve-bot

A generic event-processing engine. Out of the box it ingests the
[zKillboard](https://zkillboard.com) live killmail feed, but every stage of
the pipeline is pluggable:

```
┌────────┐   ┌──────────┐   ┌───────┐   ┌─────────┐
│ Source ├──▶│ Enrichers├──▶│ Rules ├──▶│ Actions │
└────────┘   └──────────┘   └───────┘   └─────────┘
                                            │
                                            ▼
                                   ┌────────────────┐
                                   │  SQLite facts  │
                                   │  (for later    │
                                   │   enrichment)  │
                                   └────────────────┘
```

- **Sources** produce `Event`s. Killmails, wormhole-connection updates, ESI
  polling, and Discord slash-commands are all just sources.
- **Enrichers** mutate `Event.Fields` — SDE lookups for ship / weapon / item
  names and meta levels, ESI `/v3/universe/names/` lookups for character /
  corporation / alliance names (cached in SQLite for 7 days by default),
  fact lookups from the store, anything a rule might want to read.
- **Rules** are YAML-declarative; the `when:` clause is an
  [expr-lang](https://github.com/expr-lang/expr) boolean expression compiled
  at startup.
- **Actions** are the side effects — console, webhook, fact-store writes,
  Discord replies — with idempotency and retry built in.

See [RULES.md](RULES.md) for the rule language and [spec.md](spec.md) for the
full design.

---

## Quick start

```sh
go build -o eve-bot ./cmd/eve-bot
./eve-bot                       # uses ./config.yaml
./eve-bot -config /my/cfg.yaml
```

`Ctrl+C` stops the bot; checkpoints are persisted to SQLite (`eve-bot.db`)
so it resumes where it left off.

## Tests

```sh
go test ./...
```

## Updating static game data

Ship names, item names, and solar-system names are compiled into the binary.
Rebuild them when CCP ships new content:

```sh
go run ./cmd/gen-sde         # from ./eve.db
go run ./cmd/gen-systems     # from ESI
go build -o eve-bot ./cmd/eve-bot
```

## Adding a new source (in this repo)

1. Create `internal/source/<name>/` with a `Source` implementing
   [`source.Source`](source/source.go) — `Name()` and `Run(ctx, out)`.
2. Normalize payloads into `event.Event` with `Fields` as nested
   `map[string]any` so rules can address nested values with dots
   (`zkb.total_value`).
3. Add an `init()` in the new package that calls `source.Register("<type>", factory)`.
4. Blank-import the package from `bot/defaults/defaults.go` so the stock
   binary includes it.
5. Configure it in `config.yaml` as a new `sources:` entry with
   `type: <name>`.

Stubs for `esi` and `discord` show the shape.

## Extending from a private repo

`eve-bot` is designed as a library. Private repos can depend on it as a Go
module, register their own sources and actions, and ship their own binary
without forking this one.

Layout:

```
my-private-bot/
├── go.mod                              # module my-private-bot
│                                       # require github.com/joeyciechanowicz/eve-bot
├── cmd/my-private-bot/main.go
└── internal/source/secret/secret.go    # implements source.Source
                                        # init() calls source.Register("secret", ...)
```

`cmd/my-private-bot/main.go`:

```go
package main

import (
    "context"
    "flag"
    "os"
    "os/signal"
    "syscall"

    "github.com/joeyciechanowicz/eve-bot/bot"
    _ "github.com/joeyciechanowicz/eve-bot/bot/defaults"  // stock zkill/evescout/actions
    _ "my-private-bot/internal/source/secret"             // your custom source
)

func main() {
    cfg := flag.String("config", "./config.yaml", "path to config file")
    flag.Parse()
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    if err := bot.Run(ctx, *cfg); err != nil { os.Exit(1) }
}
```

Your source implements `source.Source` and calls `source.Register` in its
`init()`; the YAML `type:` selects it. Same pattern for custom actions via
`action.Register`.

For private GitHub modules, set `GOPRIVATE=github.com/you/*` so Go skips the
public proxy.

## Requirements

- Go 1.25+
- No CGO — uses `modernc.org/sqlite`, so cross-compiling a single binary
  "just works".
