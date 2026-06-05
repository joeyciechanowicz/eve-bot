# Custom Functions in Rules & Templates — Design

**Date:** 2026-06-04
**Status:** Approved, ready for implementation planning

## Problem

Rule authors and library consumers want to reuse logic across `when:` expressions and
templated action args. Today the only shared helpers are the hard-coded `fact`, `now`,
etc. in `internal/rules/rules.go:buildEnv`. There is no way for a consumer to expose
their own functions to either the expr-lang `when:` clause or the Go `text/template`
action args.

## Goals

- Let consumers expose functions to **both** evaluation contexts:
  - `when:` expressions — evaluated by **expr-lang** (`internal/rules/rules.go`)
  - `{{ }}` templates — rendered by **Go `text/template`** (`action/action.go:walkRender`)
- Two authoring channels:
  1. **Go functions** registered programmatically (power users importing the library)
  2. **Parameterized YAML functions** declared in config (config-only authors)
- YAML function bodies may call: expr-lang builtins, the event env (current event
  fields), Go-registered functions, and **other** YAML functions.

## Non-Goals

- Recursion / mutually-recursive YAML functions. Cycles are rejected at compile time.
- A separate namespace/module system for functions. Names are flat.
- Hot-reloading function definitions at runtime. They are compiled once at startup.

## Background (current architecture)

- **Expression engine:** `github.com/expr-lang/expr`. Rules compiled once at startup in
  `rules.Compile` with `expr.AsBool()` + `expr.AllowUndefinedVariables()`. The runtime
  env is a `map[string]any` built per-event by `buildEnv` (event fields at top level
  plus helpers like `fact`, `now`).
  - Key fact: because compilation uses `AllowUndefinedVariables()`, identifiers that
    exist only in the runtime env (e.g. `fact(...)`) compile fine and resolve at run
    time. Custom functions ride the same mechanism.
- **Template engine:** Go `text/template`, rendered lazily in `action/action.go:walkRender`
  only when a string contains `{{`. The render context `ctx` is a `map[string]any` of
  event fields plus `item`, `event_id`, etc. `text/template` funcs receive only their
  args, **not** the data context — so YAML funcs that read event fields must close over
  `ctx` at render time.

## Design

### 1. Config surface

New top-level block. Signature is encoded in the YAML key; body is a plain expr-lang
string:

```yaml
functions:
  'near_jita(system, jumps)': 'distance(system, 30000142) <= jumps'
  'is_expensive(threshold)':  'zkb.total_value > threshold'
```

- Added to `bot.Config` as `Functions map[string]string ` + yaml tag `functions`.
- Each entry parses into `{ name string, params []string, body string }`.
- Bodies are closures over the current event env: in the example, `is_expensive`
  references `zkb.total_value` directly in addition to its `threshold` param.

### 2. New package: `internal/funcs`

A single compiled **function set** shared by the rules engine and the action dispatcher.

State:
- `goFuncs map[string]any` — registered Go funcs (raw Go func values).
- `yamlFuncs []yamlFunc` where `yamlFunc { name string; params []string; program *vm.Program }`
  — each body compiled once at startup via `expr.Compile(body, expr.AllowUndefinedVariables())`.

Two binding methods keep both engines DRY:

- `BindExprEnv(env map[string]any)`
  - Injects every Go func directly into `env`.
  - Wraps each YAML func as a variadic closure added to `env`. The closure **captures
    `env` by reference**; when called it clones `env`, binds the call's positional args
    to the declared param names in the clone, and runs the func's `program` against the
    clone. Because the closure reads `env` at call time (after `env` is fully populated
    with all funcs and event fields), YAML funcs can call Go funcs, call other YAML
    funcs, and read event fields.
  - Clone-per-call keeps evaluation side-effect-free across rules and calls.

- `TemplateFuncMap(ctx map[string]any) template.FuncMap`
  - Builds an expr env from the render `ctx` (so YAML funcs reach event fields and each
    other), then returns a `template.FuncMap` whose entries route through the same
    closures. Go funcs are added directly. This gives identical function behavior inside
    `{{ }}`.

### 3. Wiring

- `bot` package gains an `Option` type and `WithFunc(name string, fn any) Option`.
  `Run` and `RunConfig` accept `...Option` and collect registered Go funcs.
- `buildPipeline` compiles `cfg.Functions` together with the registered Go funcs into a
  `*funcs.Set` (fail fast on error).
- `rules.Compile` is extended to receive the `*funcs.Set`; `buildEnv` calls
  `set.BindExprEnv(env)` after copying event fields.
- `action.New` / `action.BuildHandlers` receive the `*funcs.Set`; `walkRender` calls
  `tmpl.Funcs(set.TemplateFuncMap(ctx))` before `Parse`.

### 4. Compile-time validation (fail fast at startup)

- Parse each key as a valid `ident(arg, ...)` signature; the function name and each
  param must be valid, unique identifiers.
- Reject function names that collide with reserved env identifiers (`fact`,
  `fact_exists`, `fact_count`, `now`, `event_id`, `event_source`, `event_type`,
  `occurred_at`) or with each other across the Go and YAML sets.
- Compile each YAML body; surface expr-lang compile errors prefixed with the function
  name.
- **Cycle detection:** walk each compiled body's AST for identifiers matching other
  YAML func names, build a dependency graph, and reject any cycle (no recursion).
  Topologically sort for deterministic reporting.
- Validate registered Go funcs are `text/template`-compatible: signature returns either
  `T` or `(T, error)`; variadic params allowed.

### 5. Runtime error handling

Consistent with current behavior:
- A function returning an error inside an expression → expr.Run returns an error →
  logged as `rules: eval error`, that rule is skipped (matches existing handling in
  `Evaluate`).
- A function returning an error inside a template → propagates as the existing
  template-exec error from `walkRender`.
- Per-call env cloning ensures functions cannot leak state between rules or calls.

### 6. Testing

- `internal/funcs` unit tests:
  - signature parsing (valid + malformed keys)
  - collision rejection (reserved names, duplicate names across Go/YAML)
  - cycle rejection and successful toposort of valid chains
  - Go + YAML composition (YAML body calling a Go func and another YAML func)
  - positional param binding
  - env isolation (clone — a func cannot mutate the caller's env)
  - template-context path (`TemplateFuncMap`) reaches event fields
- Integration test: a `functions:` block exercised through both a `when:` clause and a
  templated action arg in the same run.
- Extend `cmd/rule-check` to load the `functions:` block so rules that use custom
  functions validate and dry-run correctly there too.

## Open questions

None. Recursion is disallowed (cycles rejected); `cmd/rule-check` will be made
functions-aware.
