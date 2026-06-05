# Writing rules

Sources and rules are sibling top-level blocks in `config.yaml`. Every source
feeds a shared rule engine; rules may opt into a subset of sources via
`sources:`, or omit it to see events from every source.

```yaml
sources:
  - name: zkill
    type: zkill

rules:
  mode: first-match # or: multi-match
  rules:
    - name: high-value-kill
      enabled: true
      priority: 10
      sources: [zkill]          # optional; omit = every source
      when: "zkb.total_value > 1e9"
      actions:
        - type: console
```

## Fields

Top-level identifiers in a `when:` expression are the keys of `Event.Fields`
for the event being evaluated. Different sources emit different field sets —
when a rule is scoped to multiple sources, branch on `event_source` (or
`event_type`) to select the right shape.

### zkill source

| Identifier          | Type      | Notes                            |
| ------------------- | --------- | -------------------------------- |
| `killmail_id`       | int64     |                                  |
| `hash`              | string    |                                  |
| `sequence_id`       | int64     |                                  |
| `uploaded_at`       | time.Time | zkill upload time                |
| `killmail_time`     | time.Time | in-game event time               |
| `solar_system_id`   | int64     |                                  |
| `solar_system_name` | string    | from SDE enrichment              |
| `victim`            | object    | see below                        |
| `attackers`         | []object  | see below                        |
| `attacker_count`    | int       |                                  |
| `final_blow`        | object    | one entry from attackers         |
| `items`             | []object  | victim fittings/cargo            |
| `zkb`               | object    | zKillboard metadata              |
| `has_capital`       | bool      | any participant is capital-class |

**Victim / attacker / item shapes** mirror the ESI killmail payload plus
enrichment additions:

- **Ship fields** (`victim` and each `attackers[*]`): `ship_name`,
  `ship_group`, `ship_group_id`, `ship_category`. Attackers additionally get
  `meta_level` and `meta_group` for their ship type.
- **Weapon fields** (each `attackers[*]`): `weapon_name`, `weapon_group`,
  `weapon_group_id`, `weapon_category` — looked up from `weapon_type_id` via
  the SDE.
- **Item fields** (`items[*]`): `name`, `meta_level`, `meta_group`.
- **Actor name fields** (`victim`, each `attackers[*]`, and `final_blow`):
  `character_name`, `corporation_name`, `alliance_name` — resolved from
  `character_id` / `corporation_id` / `alliance_id` via the ESI
  `/v3/universe/names/` bulk endpoint and cached in the SQLite store
  (7-day TTL by default). Names for IDs of `0` are not set, so NPC kills
  (`character_id == 0`) won't have `character_name`. If ESI is unreachable
  the lookup is skipped and the name field is simply absent — write
  rules against the IDs for hard checks and use the names for templating
  webhook bodies.

### evescout source

Emits one `signature.added` event per newly reported EVE Scout wormhole
connection (Thera/Turnur). Fields:

| Identifier         | Type      | Notes                                     |
| ------------------ | --------- | ----------------------------------------- |
| `signature_id`     | string    | EVE Scout's row id                        |
| `wh_type`          | string    | wormhole class code (e.g. `K162`, `C140`) |
| `max_ship_size`    | string    | `small` / `medium` / `large` / `xlarge`   |
| `signature_type`   | string    | always `wormhole` today                   |
| `wh_exits_outward` | bool      | direction of the K162 pair                |
| `remaining_hours`  | float64   | time until collapse                       |
| `expires_at`       | time.Time |                                           |
| `created_at`       | time.Time | when EVE Scout recorded the signature     |
| `created_by_id`    | int64     |                                           |
| `created_by_name`  | string    | reporter's character name                 |
| `in`               | object    | entry system — see below                  |
| `out`              | object    | exit system — see below                   |

`in` object: `system_id` (int64), `system_name` (string), `system_class`
(string — `HS`/`LS`/`NS`/`C1`–`C6`/`C13`/`C-WH`/`Thera`), `region_id`
(int64), `region_name` (string), `signature` (string — e.g. `ABC-123`).

`out` object: `system_id` (int64), `system_name` (string), `signature`
(string).

## Built-in helpers

All rule expressions can call:

| Call                        | Returns                       |
| --------------------------- | ----------------------------- |
| `fact(scope, key)`          | `any` — JSON-decoded or `nil` |
| `fact_exists(scope, key)`   | `bool`                        |
| `fact_count(scope, prefix)` | `int`                         |
| `now()`                     | `time.Time` (UTC)             |

Plus the full expr-lang builtins: `any`, `all`, `filter`, `map`, `len`,
`contains`, `string`, `int`, date/time functions, etc. See
<https://expr-lang.org/docs/language-definition>.

## Custom functions

Declare reusable functions in a top-level `functions:` block. The key is a
signature; the value is an expr-lang body. A body can use the builtins above,
the current event's fields, any other custom function, and any Go function the
host program registered (see below).

```yaml
functions:
  'is_expensive(threshold)':  'zkb.total_value > threshold'
  'near_jita(system, jumps)': 'distance(system, 30000142) <= jumps'
```

The same functions are available in any `when:` clause **and** in any
templated action arg:

```yaml
rules:
  - name: pricey
    when: 'is_expensive(1e9)'
    actions:
      - type: console
        args:
          msg: '{{ if is_expensive 1e9 }}BIG{{ end }} kill'
```

Notes:

- A body is evaluated against the current event, so it can read fields
  directly (e.g. `zkb.total_value`) on top of its declared params.
- Functions may call one another, but **cycles are rejected at startup** —
  functions are not recursive.
- A name may not shadow a built-in helper (`fact`, `now`, …) or an event
  field reserved identifier; collisions fail fast at startup.

Go functions are registered in code when starting the bot:

```go
bot.Run(ctx, "config.yaml",
    bot.WithFunc("distance", func(a, b int64) int { /* ... */ }),
)
```

A `WithFunc` value must return either `T` or `(T, error)` so it works in both
expr-lang and `text/template`.

To validate a rule that uses custom functions, pass the same file (or any YAML
file with a `functions:` block) to `rule-check` via `--functions`:

```
rule-check --rule myrule.yaml --event fixture.json --functions config.yaml --explain
```

## Modes

- **first-match** — rules are tried in ascending `priority`; the first match
  wins. A matched rule with `continue: true` does **not** stop evaluation —
  use it for bookkeeping rules (e.g. fact writers) that should run before
  later decision rules.
- **multi-match** — every enabled rule that matches fires.

## Examples

### High-value kill

```yaml
- name: high-value
  priority: 10
  when: "zkb.total_value > 1e9"
  actions:
    - type: console
    - type: webhook
      args: { url: "https://example/hook" }
```

### Capital involvement

```yaml
- name: capital
  priority: 20
  when: "has_capital && !zkb.npc"
  actions: [{ type: console }]
```

### Record every attacker's kill (fact writer)

```yaml
- name: record-attacker-kills
  priority: 1
  continue: true # don't stop the pipeline at the writer
  when: "true"
  actions:
    - type: store
      for: attackers
      args:
        scope: kill_by_char
        key: "{{ .item.character_id }}"
        op: inc
        field: count
        by: 1
        ttl: 720h # 30d rolling window
```

### Repeat offender (reads the fact above)

```yaml
- name: repeat-offender
  priority: 30
  when: |
    any(attackers, {
      let f = fact("kill_by_char", string(.character_id));
      f != nil && f.count >= 5
    }) && !zkb.npc
  actions: [{ type: console }]
```

### Trade-hub gank

```yaml
- name: jita-hub
  priority: 40
  when: |
    solar_system_name in ["Jita", "Amarr", "Dodixie", "Rens", "Hek"]
    && zkb.total_value > 5e8
    && !zkb.npc
  actions: [{ type: console }]
```

### Alliance-scoped alert (uses ESI names)

```yaml
- name: goonswarm-loses-capital
  priority: 15
  when: |
    has_capital
    && victim.alliance_name == "Goonswarm Federation"
  actions:
    - type: webhook
      args:
        url: https://discord.com/api/webhooks/XXX/YYY
        body:
          content: >
            💀 {{.victim.character_name}} ({{.victim.corporation_name}})
            lost a {{.victim.ship_name}} in {{.solar_system_name}}
```

### Watched wormhole exit (evescout)

```yaml
- name: watched-wormhole-exit
  priority: 10
  when: 'out.system_name in ["J123456", "J234567"]'
  actions:
    - type: webhook
      args:
        url: https://discord.com/api/webhooks/XXX/YYY
        body: *discord_signature_embed   # see "Reusable action bodies" below
```

## Reusable action bodies

Action `args:` (including the `body:` sent by the `webhook` action) are
templated at dispatch time: every string containing `{{` is run through
`text/template` with the event fields at the top level, plus `event_id`,
`event_source`, `event_type`, `occurred_at`, and `item` (for `for:`
iteration).

To avoid copy-pasting long bodies across rules, use **YAML anchors**. The
config parser (`yaml.v3`) expands anchors at load time, so the code only
ever sees the fully-resolved map — no schema changes needed.

```yaml
# Park anchors under any key the Config struct doesn't declare; yaml.v3
# ignores unknown top-level keys. `x-templates` is just a convention.
x-templates:
  discord_signature_embed: &discord_signature_embed
    username: "EVE Scout Watch"
    embeds:
      - title: "🌌 {{.in.system_name}} → {{.out.system_name}}"
        color: 15844367
        timestamp: '{{.created_at.Format "2006-01-02T15:04:05Z07:00"}}'
        fields:
          - {
              name: "Entry",
              value: "[{{.in.system_name}}](https://evemaps.dotlan.net/system/{{.in.system_name}}) ({{.in.system_class}}) · {{.in.region_name}}",
              inline: true,
            }
          - {
              name: "Exit",
              value: "[{{.out.system_name}}](https://evemaps.dotlan.net/system/{{.out.system_name}})",
              inline: true,
            }
          - { name: "Type", value: "{{.wh_type}}", inline: true }
          - { name: "Max Ship", value: "{{.max_ship_size}}", inline: true }
          - {
              name: "Lifetime",
              value: '{{printf "%.1f" .remaining_hours}}h',
              inline: true,
            }
          - { name: "Reporter", value: "{{.created_by_name}}", inline: true }
        footer:
          text: "EVE Scout • {{.signature_id}}"

sources:
  - name: evescout
    type: evescout
    poll_interval: 60s

rules:
  mode: first-match
  rules:
    - name: watched-wormhole-exit
      sources: [evescout]
      when: 'out.system_name in ["J123456", "J234567"]'
      actions:
        - type: webhook
          args:
            url: https://discord.com/api/webhooks/AAA/BBB
            body: *discord_signature_embed

    - name: thera-jita-bridge
      sources: [evescout]
      when: 'out.system_name == "Jita"'
      actions:
        - type: webhook
          args:
            url: https://discord.com/api/webhooks/CCC/DDD
            body: *discord_signature_embed
```

## Cross-source rules

Because every source feeds the same engine and the same SQLite fact store,
one rule can write a fact on a kill and a different rule can read that fact
on a signature event. Example: remember high-value wormhole kills, then
alert when EVE Scout reports a Thera connection into the same system.

```yaml
rules:
  mode: multi-match
  rules:
    - name: record-wh-high-value
      sources: [zkill]
      when: 'zkb.total_value > 1e9 && solar_system_name matches "^J\\d{6}$"'
      actions:
        - type: store
          args:
            scope: wh_target
            key: "{{ .solar_system_id }}"
            op: set
            field: total_value
            value: "{{ .zkb.total_value }}"
            ttl: 720h

    - name: thera-connection-to-target
      sources: [evescout]
      when: 'fact_exists("wh_target", string(in.system_id))'
      actions:
        - type: console
```

**Overriding fields in the template** per-rule uses a YAML merge key:

```yaml
body:
  <<: *discord_signature_embed
  username: "Thera Bridge Watch"
```

**Rules for anchors**

- Define the anchor (`&name`) **before** its first reference (`*name`) in
  the document.
- `x-templates:` must not collide with any real top-level key. It won't,
  because the `Config` struct doesn't declare it.
- Templating still happens on the resolved body, so `{{...}}` inside the
  anchor renders per-event at dispatch time — shared structure, per-event
  values.

## Validation

All `when:` expressions are compiled at startup; a syntax error fails the
process with the rule name in the message. Undefined field names are
permitted at compile time and silently yield `nil` at eval — watch for typos.
