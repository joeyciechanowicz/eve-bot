# Writing a zkill-bot rule

Guide for any coding agent (or human) authoring or modifying a rule in `config.yaml`. A rule is a YAML block with a boolean `when:` expression (expr-lang) plus a list of actions under `pipelines[].rules`. The full schema, field surface, and helpers are in [`RULES.md`](RULES.md) — read it first, don't reproduce it from memory. This doc is the *process*; `RULES.md` is the *reference*.

## The loop

1. **Clarify intent.** If the user says "alert on big kills", pin down the threshold, whether NPC kills count, and which actions fire. One question is fine; three is too many.
2. **Read `RULES.md`** for the field names and helpers available to the source you're targeting. Supported sources: `zkill` (killmails) and `evescout` (wormhole signatures). Field shapes differ — a `solar_system_name` rule makes no sense on an evescout pipeline, and `out.system_name` makes none on a zkill pipeline.
3. **Draft the rule** in isolation — write it to a scratch file like `/tmp/rule.yaml`, not directly into `config.yaml`. A single rule, not the whole pipeline.
4. **Validate** with `go run ./cmd/rule-check --rule /tmp/rule.yaml --event testdata/<fixture>.json`. Run it against *every* fixture under `testdata/killmails/` that's relevant — at minimum one expected match and one expected non-match.
5. **Inspect sub-expressions** when behavior is wrong: `--explain` prints the value of each top-level identifier in the `when:` clause so you can see whether a field came back `nil` (typo) or a different type than expected.
6. **Insert** into `config.yaml` only after validation passes. Pick a `priority` that fits the existing ordering — lower numbers run first; bookkeeping/fact-writer rules want low priorities with `continue: true`.

## Validator contract

`cmd/rule-check` accepts:
- `--rule <path>` — a YAML file containing a single rule (same shape as one entry in the `rules:` list).
- `--event <path>` — a JSON file containing one event's `Fields` payload (matches a fixture in `testdata/`).
- `--fact scope:key=<json>` — repeatable; seeds the fact store for rules that read facts.
- `--explain` — print each identifier referenced by `when:` and its resolved value.

Exit codes: `0` match, `1` no-match, `2` compile/runtime error. Treat `2` as a bug to fix, not a "no match".

## Fixtures

`testdata/killmails/` holds zkill fixtures. `testdata/signatures/` holds evescout fixtures (hand-maintained; small enough that the schema-drift risk is low). Both directories contain captured post-enrichment events as JSON (the same `Event.Fields` shape rules see).

**Note on names:** `cmd/capture-fixture` runs the source-local enrich (SDE lookups for ship/weapon/item names and meta levels) but **not** the pipeline-level ESI names enricher, so fixtures won't contain `character_name` / `corporation_name` / `alliance_name`. Validate name-based rules against the IDs (`victim.alliance_id == 1354830081`) when running `cmd/rule-check` locally; the name fields will be present at runtime but `nil` in the fixture.

zkill fixtures:

| File                | Scenario                                              |
|---------------------|-------------------------------------------------------|
| `capital.json`      | Dreadnought loss, multi-attacker, `has_capital=true`  |
| `high_value.json`   | ~600M PvP destroyer loss, no capital, no NPC          |
| `npc_kill.json`     | Cruiser killed by NPCs, `zkb.npc=true`                |
| `pod.json`          | Capsule loss with expensive implants                  |
| `solo_frigate.json` | Cheap frigate, `zkb.solo=true`, single attacker       |

evescout fixtures:

| File                              | Scenario                                             |
|-----------------------------------|------------------------------------------------------|
| `thera_to_j123456.json`           | J-space exit (matches `out.system_name in [...]`)    |
| `thera_to_jita.json`              | Highsec exit to Jita (negative case for J-space)     |

**Adding a fixture** when a rule needs a scenario not covered above: use `cmd/capture-fixture`, which taps the live zkill source and writes the first matching enriched event to disk. Never hand-write fixtures — synthetic ones drift from the real schema.

```
go run ./cmd/capture-fixture \
  --predicate 'solar_system_name == "Jita" && attacker_count >= 3 && !zkb.npc' \
  --out testdata/killmails/jita_gank.json \
  --timeout 30m
```

The predicate uses the same expr-lang environment as rules. Common gaps worth capturing when you hit them: `jita_gank.json` (Jita + multi-attacker PvP), `awox.json` (`zkb.awox`), `t1_industrial.json` (mining ganks).

## Common mistakes

- **Typo'd field names silently return `nil`.** `zkb.totalvalue > 1e9` compiles fine and never matches. `--explain` is the fastest way to spot this.
- **Fact keys must be strings.** `fact("kill_by_char", character_id)` fails because `character_id` is an int — wrap with `string(...)`.
- **First-match ordering.** A new rule placed at a higher priority than an existing catch-all will never fire. Check the surrounding `priority:` values in `config.yaml` before picking one.
- **Fact writers need `continue: true`.** Without it, the writer matches and stops the pipeline before the decision rules run.
- **`!zkb.npc` on alerting rules.** NPC kills are noisy; almost every "interesting human activity" rule wants this guard. Ask the user if you're unsure.
- **Long action bodies should use YAML anchors.** If a rule's `args.body:` (typically a Discord embed or other webhook payload) is likely to be reused across rules, define it once under `x-templates:` with `&name` and reference it via `*name`. See the "Reusable action bodies" section in `RULES.md`. Don't inline a 30-line embed into three rules.
- **ESI name fields are best-effort.** `character_name` / `corporation_name` / `alliance_name` come from the ESI bulk-names endpoint and are cached for 7 days. If ESI is unreachable on the very first sighting of an ID, the name is absent (rules see `nil`). For hard equality checks prefer the underlying ID (`victim.alliance_id == 1354830081`); reserve names for templated action bodies where a missing name is cosmetic, not load-bearing.

## Output to the user

After validation, show:
- The final rule YAML.
- Which fixtures it matched and which it didn't.
- Where in `config.yaml` you placed it (or are proposing to place it) and why that priority.

Do not summarize what the rule "does" in prose — the YAML is the contract.
