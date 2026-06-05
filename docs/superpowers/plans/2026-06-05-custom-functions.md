# Custom Functions in Rules & Templates — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let consumers expose custom functions — Go-registered and YAML-declared (parameterized) — to both `when:` expr-lang expressions and `{{ }}` text/template action args.

**Architecture:** A new `internal/funcs` package compiles a shared function `Set` (Go funcs + YAML funcs) once at startup. The set binds into the expr-lang runtime env (`internal/rules`) and into a `text/template.FuncMap` (`action`). YAML func bodies are expr-lang programs that close over the per-event env, so they can read event fields and call Go funcs and other YAML funcs. Cycles are rejected at compile time.

**Tech Stack:** Go, `github.com/expr-lang/expr` (+ `/ast`, `/parser`, `/vm`), `text/template`, `gopkg.in/yaml.v3`. Module path: `github.com/joeyciechanowicz/eve-bot`.

**Spec:** `docs/superpowers/specs/2026-06-04-custom-functions-design.md`

---

## File Structure

- **Create** `internal/funcs/funcs.go` — `Set`, `Compile`, signature parsing, validation, cycle detection, `BindExprEnv`, `TemplateFuncMap`.
- **Create** `internal/funcs/funcs_test.go` — unit tests.
- **Create** `internal/funcs/integration_test.go` — end-to-end through rules + template.
- **Modify** `bot/config.go` — add `Functions map[string]string` field.
- **Modify** `internal/rules/rules.go` — `Compile` takes `*funcs.Set`; `buildEnv` binds it.
- **Modify** `internal/rules/rules_test.go` — update `Compile` call sites (pass `nil`).
- **Modify** `action/action.go` — `New` takes `*funcs.Set`; render path uses `TemplateFuncMap`.
- **Modify** `action/action_test.go` — update `New` call sites (pass `nil`).
- **Modify** `bot/run.go` — `Option`/`WithFunc`, thread funcs through `buildPipeline`.
- **Modify** `cmd/rule-check/main.go` — `--functions` flag, thread set, explain skip-list.

---

## Task 1: Add `Functions` field to Config

**Files:**
- Modify: `bot/config.go:36-40`
- Test: `bot/config_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `bot/config_test.go`:

```go
package bot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigParsesFunctions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `
store:
  path: ./test.db
sources:
  - name: zkill
    type: zkill
functions:
  'near_jita(system, jumps)': 'distance(system, 30000142) <= jumps'
  'is_expensive(threshold)': 'zkb.total_value > threshold'
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.Functions["near_jita(system, jumps)"]; got != "distance(system, 30000142) <= jumps" {
		t.Fatalf("near_jita body = %q", got)
	}
	if len(cfg.Functions) != 2 {
		t.Fatalf("want 2 functions, got %d", len(cfg.Functions))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./bot/ -run TestLoadConfigParsesFunctions -v`
Expected: FAIL — `cfg.Functions` undefined (compile error).

- [ ] **Step 3: Add the field**

In `bot/config.go`, add to the `Config` struct after the `Enrich` field (around line 39):

```go
	Enrich EnrichConfig `yaml:"enrich"`

	// Functions declares custom functions usable in both `when:` expressions
	// and templated action args. The key is a signature ("name(a, b)") and the
	// value is an expr-lang body. See internal/funcs.
	Functions map[string]string `yaml:"functions"`
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./bot/ -run TestLoadConfigParsesFunctions -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add bot/config.go bot/config_test.go
git commit -m "feat(config): add functions block to Config"
```

---

## Task 2: `internal/funcs` — signature parsing & validation

**Files:**
- Create: `internal/funcs/funcs.go`
- Test: `internal/funcs/funcs_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/funcs/funcs_test.go`:

```go
package funcs

import (
	"strings"
	"testing"
)

func TestParseSignature(t *testing.T) {
	cases := []struct {
		key     string
		name    string
		params  []string
		wantErr bool
	}{
		{"near_jita(system, jumps)", "near_jita", []string{"system", "jumps"}, false},
		{"noargs()", "noargs", nil, false},
		{"  spaced ( a , b )", "spaced", []string{"a", "b"}, false},
		{"missing_parens", "", nil, true},
		{"1bad(a)", "", nil, true},
		{"dup(a, a)", "", nil, true},
		{"f(1)", "", nil, true},
	}
	for _, c := range cases {
		name, params, err := parseSignature(c.key)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got none", c.key)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.key, err)
			continue
		}
		if name != c.name || strings.Join(params, ",") != strings.Join(c.params, ",") {
			t.Errorf("%q: got (%q, %v), want (%q, %v)", c.key, name, params, c.name, c.params)
		}
	}
}

func TestCompileRejectsReservedName(t *testing.T) {
	_, err := Compile(nil, map[string]string{"now()": "true"})
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("want reserved-name error, got %v", err)
	}
}

func TestCompileRejectsGoYamlCollision(t *testing.T) {
	goFns := map[string]any{"dup": func() bool { return true }}
	_, err := Compile(goFns, map[string]string{"dup(a)": "a > 0"})
	if err == nil || !strings.Contains(err.Error(), "dup") {
		t.Fatalf("want collision error, got %v", err)
	}
}

func TestCompileRejectsNonTemplateGoFunc(t *testing.T) {
	// Two non-error returns is not text/template-compatible.
	goFns := map[string]any{"bad": func() (int, int) { return 1, 2 }}
	_, err := Compile(goFns, nil)
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("want signature error, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/funcs/ -v`
Expected: FAIL — package `funcs` does not exist.

- [ ] **Step 3: Write the implementation**

Create `internal/funcs/funcs.go`:

```go
// Package funcs compiles a set of custom functions usable in both expr-lang
// `when:` expressions (internal/rules) and Go text/template action args
// (action). Functions come from two sources: Go funcs registered by the host
// program, and parameterized YAML funcs whose bodies are expr-lang programs.
//
// YAML func bodies close over the per-event environment, so they may read event
// fields and call Go funcs and other YAML funcs. Cycles among YAML funcs are
// rejected at compile time (no recursion).
package funcs

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
	"github.com/expr-lang/expr/vm"
)

// reserved identifiers are provided by internal/rules.buildEnv (and the
// template render context). Custom functions may not shadow them. Keep this in
// sync with internal/rules/rules.go:buildEnv and action/action.go:renderArgs.
var reserved = map[string]bool{
	"event_id": true, "event_source": true, "event_type": true,
	"occurred_at": true, "now": true,
	"fact": true, "fact_exists": true, "fact_count": true,
	"item": true,
}

// yamlFunc is one compiled YAML-declared function.
type yamlFunc struct {
	name    string
	params  []string
	program *vm.Program
}

// Set is a compiled, validated collection of custom functions.
type Set struct {
	goFuncs   map[string]any
	yamlFuncs []yamlFunc
}

// Compile validates and compiles the function set. goFuncs maps a name to a
// raw Go func value (must be text/template-compatible: one return value, or a
// value plus error). yamlSrc maps a signature key ("name(a, b)") to an
// expr-lang body. Compile fails fast on any error.
func Compile(goFuncs map[string]any, yamlSrc map[string]string) (*Set, error) {
	s := &Set{goFuncs: map[string]any{}}

	// Validate Go funcs.
	for name, fn := range goFuncs {
		if !isIdent(name) {
			return nil, fmt.Errorf("funcs: invalid Go func name %q", name)
		}
		if reserved[name] {
			return nil, fmt.Errorf("funcs: %q is a reserved identifier", name)
		}
		if err := validateGoFunc(name, fn); err != nil {
			return nil, err
		}
		s.goFuncs[name] = fn
	}

	// Parse + compile YAML funcs.
	names := map[string]bool{}
	for k := range s.goFuncs {
		names[k] = true
	}
	for key, body := range yamlSrc {
		name, params, err := parseSignature(key)
		if err != nil {
			return nil, fmt.Errorf("funcs: %w", err)
		}
		if reserved[name] {
			return nil, fmt.Errorf("funcs: %q is a reserved identifier", name)
		}
		if names[name] {
			return nil, fmt.Errorf("funcs: duplicate function name %q", name)
		}
		names[name] = true
		prog, err := expr.Compile(body, expr.AllowUndefinedVariables())
		if err != nil {
			return nil, fmt.Errorf("funcs: compile %q: %w", name, err)
		}
		s.yamlFuncs = append(s.yamlFuncs, yamlFunc{name: name, params: params, program: prog})
	}

	if err := s.checkAcyclic(yamlSrc); err != nil {
		return nil, err
	}
	return s, nil
}

// validateGoFunc ensures fn is a func returning either (T) or (T, error).
func validateGoFunc(name string, fn any) error {
	t := reflect.TypeOf(fn)
	if t == nil || t.Kind() != reflect.Func {
		return fmt.Errorf("funcs: %q is not a function", name)
	}
	errType := reflect.TypeOf((*error)(nil)).Elem()
	switch t.NumOut() {
	case 1:
		return nil
	case 2:
		if t.Out(1).Implements(errType) {
			return nil
		}
	}
	return fmt.Errorf("funcs: %q must return (T) or (T, error)", name)
}

// parseSignature splits "name(a, b)" into its name and parameter list. Params
// must be unique identifiers; an empty list ("f()") is allowed.
func parseSignature(key string) (string, []string, error) {
	key = strings.TrimSpace(key)
	open := strings.IndexByte(key, '(')
	if open < 0 || !strings.HasSuffix(key, ")") {
		return "", nil, fmt.Errorf("invalid signature %q: want name(params...)", key)
	}
	name := strings.TrimSpace(key[:open])
	if !isIdent(name) {
		return "", nil, fmt.Errorf("invalid function name %q", name)
	}
	inner := strings.TrimSpace(key[open+1 : len(key)-1])
	var params []string
	if inner != "" {
		seen := map[string]bool{}
		for _, p := range strings.Split(inner, ",") {
			p = strings.TrimSpace(p)
			if !isIdent(p) {
				return "", nil, fmt.Errorf("function %s: invalid param %q", name, p)
			}
			if seen[p] {
				return "", nil, fmt.Errorf("function %s: duplicate param %q", name, p)
			}
			seen[p] = true
			params = append(params, p)
		}
	}
	return name, params, nil
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if i > 0 {
			ok = ok || (r >= '0' && r <= '9')
		}
		if !ok {
			return false
		}
	}
	return true
}

// checkAcyclic builds a call graph over YAML funcs (an edge a->b when a's body
// references b) and rejects any cycle. Calls into Go funcs and builtins are
// ignored — only YAML-to-YAML edges can form a cycle.
func (s *Set) checkAcyclic(yamlSrc map[string]string) error {
	yamlNames := map[string]bool{}
	for _, f := range s.yamlFuncs {
		yamlNames[f.name] = true
	}
	// Map name -> body so we can re-parse for identifier references.
	bodyByName := map[string]string{}
	for key, body := range yamlSrc {
		name, _, err := parseSignature(key)
		if err != nil {
			return err // already validated, but keep total
		}
		bodyByName[name] = body
	}

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(name string) error
	visit = func(name string) error {
		color[name] = gray
		tree, err := parser.Parse(bodyByName[name])
		if err != nil {
			return fmt.Errorf("funcs: parse %q: %w", name, err)
		}
		refs := map[string]struct{}{}
		node := tree.Node
		ast.Walk(&node, &identCollector{idents: refs})
		for ref := range refs {
			if !yamlNames[ref] || ref == name {
				if ref == name {
					return fmt.Errorf("funcs: function %q is recursive (cycles not allowed)", name)
				}
				continue
			}
			switch color[ref] {
			case gray:
				return fmt.Errorf("funcs: cycle detected involving %q and %q", name, ref)
			case white:
				if err := visit(ref); err != nil {
					return err
				}
			}
		}
		color[name] = black
		return nil
	}
	for _, f := range s.yamlFuncs {
		if color[f.name] == white {
			if err := visit(f.name); err != nil {
				return err
			}
		}
	}
	return nil
}

type identCollector struct {
	idents map[string]struct{}
}

func (c *identCollector) Visit(node *ast.Node) {
	if id, ok := (*node).(*ast.IdentifierNode); ok {
		c.idents[id.Value] = struct{}{}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/funcs/ -v`
Expected: PASS (TestParseSignature, TestCompileRejectsReservedName, TestCompileRejectsGoYamlCollision, TestCompileRejectsNonTemplateGoFunc).

- [ ] **Step 5: Commit**

```bash
git add internal/funcs/funcs.go internal/funcs/funcs_test.go
git commit -m "feat(funcs): compile + validate custom function set"
```

---

## Task 3: `internal/funcs` — cycle detection test

**Files:**
- Test: `internal/funcs/funcs_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/funcs/funcs_test.go`:

```go
func TestCompileRejectsCycle(t *testing.T) {
	_, err := Compile(nil, map[string]string{
		"a(x)": "b(x)",
		"b(x)": "a(x)",
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestCompileRejectsSelfRecursion(t *testing.T) {
	_, err := Compile(nil, map[string]string{"a(x)": "a(x)"})
	if err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Fatalf("want recursion error, got %v", err)
	}
}

func TestCompileAllowsAcyclicChain(t *testing.T) {
	_, err := Compile(nil, map[string]string{
		"a(x)": "b(x) + 1",
		"b(x)": "c(x) * 2",
		"c(x)": "x + 1",
	})
	if err != nil {
		t.Fatalf("acyclic chain should compile, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `go test ./internal/funcs/ -run TestCompile -v`
Expected: PASS — `checkAcyclic` (implemented in Task 2) already handles these.

> Note: cycle logic was written in Task 2. This task locks the behavior with explicit tests. If any test fails, the bug is in `checkAcyclic`; fix it there.

- [ ] **Step 3: Commit**

```bash
git add internal/funcs/funcs_test.go
git commit -m "test(funcs): cover cycle + recursion rejection"
```

---

## Task 4: `internal/funcs` — `BindExprEnv` (expr composition)

**Files:**
- Modify: `internal/funcs/funcs.go`
- Test: `internal/funcs/funcs_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/funcs/funcs_test.go`:

```go
import_marker_for_expr := 0 // placeholder; real import added below
_ = import_marker_for_expr
```

Actually add this test function (and ensure `"github.com/expr-lang/expr"` is imported in the test file):

```go
func runExpr(t *testing.T, s *Set, src string, env map[string]any) any {
	t.Helper()
	s.BindExprEnv(env)
	prog, err := expr.Compile(src, expr.AllowUndefinedVariables())
	if err != nil {
		t.Fatalf("compile %q: %v", src, err)
	}
	out, err := expr.Run(prog, env)
	if err != nil {
		t.Fatalf("run %q: %v", src, err)
	}
	return out
}

func TestBindExprEnv_GoFunc(t *testing.T) {
	s, err := Compile(map[string]any{
		"double": func(n int) int { return n * 2 },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := runExpr(t, s, "double(21)", map[string]any{})
	if got != 42 {
		t.Fatalf("double(21) = %v, want 42", got)
	}
}

func TestBindExprEnv_YamlFuncReadsEventField(t *testing.T) {
	s, err := Compile(nil, map[string]string{
		"is_expensive(threshold)": "value > threshold",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := runExpr(t, s, "is_expensive(100)", map[string]any{"value": 250})
	if got != true {
		t.Fatalf("is_expensive(100) with value=250 = %v, want true", got)
	}
}

func TestBindExprEnv_YamlCallsGoAndYaml(t *testing.T) {
	s, err := Compile(
		map[string]any{"inc": func(n int) int { return n + 1 }},
		map[string]string{
			"a(x)": "b(x) + inc(x)", // b(x)=x*2 ; inc(x)=x+1
			"b(x)": "x * 2",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := runExpr(t, s, "a(10)", map[string]any{}) // 20 + 11 = 31
	if got != 31 {
		t.Fatalf("a(10) = %v, want 31", got)
	}
}

func TestBindExprEnv_CloneIsolation(t *testing.T) {
	// A func binding its param must not leak into the caller's env.
	s, err := Compile(nil, map[string]string{"id(x)": "x"})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]any{}
	runExpr(t, s, "id(5)", env)
	if _, leaked := env["x"]; leaked {
		t.Fatal("param x leaked into caller env")
	}
}

func TestBindExprEnv_ArgCountMismatch(t *testing.T) {
	s, err := Compile(nil, map[string]string{"f(a, b)": "a + b"})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]any{}
	s.BindExprEnv(env)
	prog, _ := expr.Compile("f(1)", expr.AllowUndefinedVariables())
	if _, err := expr.Run(prog, env); err == nil {
		t.Fatal("want arg-count error, got nil")
	}
}
```

Add the test-file import:

```go
import (
	"strings"
	"testing"

	"github.com/expr-lang/expr"
)
```

(Remove the `import_marker_for_expr` placeholder lines — they were only a reminder.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/funcs/ -run TestBindExprEnv -v`
Expected: FAIL — `BindExprEnv` is undefined.

- [ ] **Step 3: Implement `BindExprEnv`**

Append to `internal/funcs/funcs.go`:

```go
import (
	"maps" // add to the existing import block, not a second block
)

// BindExprEnv injects the function set into an expr-lang environment map. env
// must already hold the event fields and reserved helpers. Go funcs are added
// directly; each YAML func is added as a variadic closure that captures env by
// reference — so when called it sees every event field and every other func.
func (s *Set) BindExprEnv(env map[string]any) {
	if s == nil {
		return
	}
	for name, fn := range s.goFuncs {
		env[name] = fn
	}
	for _, yf := range s.yamlFuncs {
		env[yf.name] = makeClosure(yf, env)
	}
}

// makeClosure builds the runtime closure for a YAML func. It clones the shared
// env per call, binds positional args to the declared params, and runs the
// compiled body. Cloning keeps calls side-effect-free and prevents param
// bindings from leaking back to the caller.
func makeClosure(yf yamlFunc, env map[string]any) func(...any) (any, error) {
	return func(args ...any) (any, error) {
		if len(args) != len(yf.params) {
			return nil, fmt.Errorf("function %s expects %d args, got %d", yf.name, len(yf.params), len(args))
		}
		child := maps.Clone(env)
		for i, p := range yf.params {
			child[p] = args[i]
		}
		return expr.Run(yf.program, child)
	}
}
```

> Implementation note: add `"maps"` to the single existing import block at the top of `funcs.go`; do not introduce a second `import (...)` group.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/funcs/ -run TestBindExprEnv -v`
Expected: PASS (all five TestBindExprEnv_* cases).

- [ ] **Step 5: Commit**

```bash
git add internal/funcs/funcs.go internal/funcs/funcs_test.go
git commit -m "feat(funcs): bind function set into expr env"
```

---

## Task 5: `internal/funcs` — `TemplateFuncMap`

**Files:**
- Modify: `internal/funcs/funcs.go`
- Test: `internal/funcs/funcs_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/funcs/funcs_test.go` (add `"bytes"` and `"text/template"` to the test imports):

```go
func renderTmpl(t *testing.T, s *Set, src string, ctx map[string]any) string {
	t.Helper()
	tmpl, err := template.New("").Funcs(s.TemplateFuncMap(ctx)).Option("missingkey=zero").Parse(src)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		t.Fatalf("exec %q: %v", src, err)
	}
	return buf.String()
}

func TestTemplateFuncMap_GoFunc(t *testing.T) {
	s, err := Compile(map[string]any{
		"upper": func(x string) string { return strings.ToUpper(x) },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := renderTmpl(t, s, `{{ upper "hi" }}`, map[string]any{})
	if got != "HI" {
		t.Fatalf("got %q, want HI", got)
	}
}

func TestTemplateFuncMap_YamlReadsCtx(t *testing.T) {
	s, err := Compile(nil, map[string]string{
		"is_expensive(threshold)": "value > threshold",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := renderTmpl(t, s, `{{ if is_expensive 100 }}yes{{ else }}no{{ end }}`,
		map[string]any{"value": 250})
	if got != "yes" {
		t.Fatalf("got %q, want yes", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/funcs/ -run TestTemplateFuncMap -v`
Expected: FAIL — `TemplateFuncMap` is undefined.

- [ ] **Step 3: Implement `TemplateFuncMap`**

Append to `internal/funcs/funcs.go` (add `"text/template"` to the import block):

```go
// TemplateFuncMap returns a text/template FuncMap for one render context. Go
// funcs are exposed directly; YAML funcs are bound through an expr env derived
// from ctx, so inside a template they can still read event fields and call
// peer funcs — exactly as they do in `when:` expressions.
func (s *Set) TemplateFuncMap(ctx map[string]any) template.FuncMap {
	fm := template.FuncMap{}
	if s == nil {
		return fm
	}
	env := maps.Clone(ctx)
	s.BindExprEnv(env)
	for name := range s.goFuncs {
		fm[name] = env[name]
	}
	for _, yf := range s.yamlFuncs {
		fm[yf.name] = env[yf.name]
	}
	return fm
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/funcs/ -v`
Expected: PASS (all funcs tests).

- [ ] **Step 5: Commit**

```bash
git add internal/funcs/funcs.go internal/funcs/funcs_test.go
git commit -m "feat(funcs): expose function set as text/template FuncMap"
```

---

## Task 6: Wire funcs into the rules engine

**Files:**
- Modify: `internal/rules/rules.go:69,116,120,154`
- Modify: `internal/rules/rules_test.go:39,48,62,79`
- Test: `internal/rules/rules_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/rules/rules_test.go` (ensure `"github.com/joeyciechanowicz/eve-bot/internal/funcs"` and `"github.com/joeyciechanowicz/eve-bot/event"` are imported — check the file's existing imports and add what's missing):

```go
func TestEvaluateUsesCustomFunc(t *testing.T) {
	fns, err := funcs.Compile(nil, map[string]string{
		"is_expensive(threshold)": "total_value > threshold",
	})
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rules.Compile(rules.ModeMultiMatch, []rules.Rule{
		{Name: "expensive", Enabled: true, When: "is_expensive(1000)"},
	}, fns)
	if err != nil {
		t.Fatal(err)
	}
	ev := &event.Event{ID: "x", Source: "zkill", Type: "killmail",
		Fields: map[string]any{"total_value": 5000}}
	if got := rs.Evaluate(ev, nil); len(got) != 1 {
		t.Fatalf("want 1 match, got %d", len(got))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/rules/ -run TestEvaluateUsesCustomFunc -v`
Expected: FAIL — `rules.Compile` takes 2 args, not 3 (compile error across the package's tests).

- [ ] **Step 3: Change `Compile` and `buildEnv` signatures**

In `internal/rules/rules.go`:

Add to the import block:
```go
	"github.com/joeyciechanowicz/eve-bot/internal/funcs"
```

Add an `fns` field to `Set` (after line 57):
```go
type Set struct {
	Mode  Mode
	Rules []Rule
	fns   *funcs.Set
}
```

Change `Compile`'s signature (line 69) and the returned `Set` (line 105):
```go
func Compile(mode Mode, raw []Rule, fns *funcs.Set) (*Set, error) {
```
```go
	return &Set{Mode: mode, Rules: out, fns: fns}, nil
```

Change `Evaluate`'s call to `buildEnv` (line 120):
```go
	env := buildEnv(ev, facts, s.fns)
```

Change `buildEnv`'s signature and add the bind (lines 154-173). Update the doc comment's reserved-identifier note to mention custom funcs:
```go
func buildEnv(ev *event.Event, facts FactStore, fns *funcs.Set) map[string]any {
	env := make(map[string]any, len(ev.Fields)+8)
	maps.Copy(env, ev.Fields)
	env["event_id"] = ev.ID
	env["event_source"] = ev.Source
	env["event_type"] = ev.Type
	env["occurred_at"] = ev.OccurredAt
	env["now"] = func() time.Time { return time.Now().UTC() }

	if facts != nil {
		env["fact"] = func(scope, key string) any { return facts.GetAny(scope, key) }
		env["fact_exists"] = func(scope, key string) bool { return facts.Exists(scope, key) }
		env["fact_count"] = func(scope, prefix string) int { return facts.RangeCount(scope, prefix) }
	} else {
		env["fact"] = func(string, string) any { return nil }
		env["fact_exists"] = func(string, string) bool { return false }
		env["fact_count"] = func(string, string) int { return 0 }
	}
	fns.BindExprEnv(env) // nil-safe
	return env
}
```

- [ ] **Step 4: Update existing `Compile` call sites in the rules test**

In `internal/rules/rules_test.go`, add `, nil` as the third argument to each existing `rules.Compile(...)` call at lines 39, 48, 62, 79. Example for line 39:
```go
	_, err := rules.Compile(rules.ModeFirstMatch, []rules.Rule{
		// ...unchanged...
	}, nil)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/rules/ -v`
Expected: PASS (existing tests + TestEvaluateUsesCustomFunc).

- [ ] **Step 6: Commit**

```bash
git add internal/rules/rules.go internal/rules/rules_test.go
git commit -m "feat(rules): bind custom functions into expression env"
```

---

## Task 7: Wire funcs into the action dispatcher

**Files:**
- Modify: `action/action.go:57,69-75,77,96-97,158,167,175-189`
- Modify: `action/action_test.go:58,88,122,155`
- Test: `action/action_test.go`

- [ ] **Step 1: Write the failing test**

Append to `action/action_test.go`. First inspect the file's existing imports and a working `New(...)` call (around line 58) to mirror its handler/store setup. Then add:

```go
func TestRenderArgsUsesCustomFunc(t *testing.T) {
	fns, err := funcs.Compile(map[string]any{
		"upper": func(s string) string { return strings.ToUpper(s) },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := &event.Event{
		ID: "x", Source: "zkill", Type: "killmail",
		Fields: map[string]any{"name": "jita"},
	}
	args := map[string]any{"text": "{{ upper .name }}"}
	out, err := renderArgs(args, ev, nil, fns)
	if err != nil {
		t.Fatal(err)
	}
	if out["text"] != "JITA" {
		t.Fatalf("text = %v, want JITA", out["text"])
	}
}
```

Add `"strings"`, `"github.com/joeyciechanowicz/eve-bot/internal/funcs"`, and (if absent) `"github.com/joeyciechanowicz/eve-bot/event"` to the test imports. This test is in `package action` (white-box) so it can call the unexported `renderArgs`; confirm the test file's `package` clause is `action` (not `action_test`). If it is `action_test`, instead test through the exported `Dispatch` path with a capturing handler.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./action/ -run TestRenderArgsUsesCustomFunc -v`
Expected: FAIL — `renderArgs` takes 3 args, not 4 (compile error).

- [ ] **Step 3: Thread `*funcs.Set` through the dispatcher**

In `action/action.go`:

Add to the import block:
```go
	"github.com/joeyciechanowicz/eve-bot/internal/funcs"
```

Add an `fns` field to `Dispatcher` (after line 53):
```go
	idem        IdempotencyStore
	fns         *funcs.Set
	Counters    Counters
```

Change `New` (line 57) to accept and store the set:
```go
func New(handlers map[string]Handler, idem IdempotencyStore, maxRetries int, baseBackoff, maxBackoff time.Duration, fns *funcs.Set) *Dispatcher {
	return &Dispatcher{
		handlers:    handlers,
		maxRetries:  maxRetries,
		baseBackoff: baseBackoff,
		maxBackoff:  maxBackoff,
		idem:        idem,
		fns:         fns,
	}
}
```

In `runAction`, pass `d.fns` to `renderArgs` (line 97):
```go
		args, err := renderArgs(ac.Args, ev, item, d.fns)
```

Change `renderArgs` to build the FuncMap once and pass it down (lines 158-173):
```go
func renderArgs(args map[string]any, ev *event.Event, item any, fns *funcs.Set) (map[string]any, error) {
	ctx := make(map[string]any, len(ev.Fields)+5)
	maps.Copy(ctx, ev.Fields)
	ctx["event_id"] = ev.ID
	ctx["event_source"] = ev.Source
	ctx["event_type"] = ev.Type
	ctx["occurred_at"] = ev.OccurredAt
	ctx["item"] = item

	fm := fns.TemplateFuncMap(ctx) // nil-safe: returns an empty FuncMap
	out, err := walkRender(args, ctx, fm)
	if err != nil {
		return nil, err
	}
	m, _ := out.(map[string]any)
	return m, nil
}
```

Change `walkRender` to accept and use the FuncMap (lines 175-213). Only the signature, the recursive calls, and the `template.New` line change:
```go
func walkRender(v any, ctx map[string]any, fm template.FuncMap) (any, error) {
	switch x := v.(type) {
	case string:
		if !bytes.Contains([]byte(x), []byte("{{")) {
			return x, nil
		}
		tmpl, err := template.New("").Funcs(fm).Option("missingkey=zero").Parse(x)
		if err != nil {
			return nil, fmt.Errorf("parse template %q: %w", x, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, ctx); err != nil {
			return nil, fmt.Errorf("exec template %q: %w", x, err)
		}
		return buf.String(), nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			r, err := walkRender(vv, ctx, fm)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			r, err := walkRender(vv, ctx, fm)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}
```

> Note: `TemplateFuncMap` must be nil-safe (it is — Task 5 returns an empty map when the receiver is nil). `template.New("").Funcs(emptyMap)` is valid.

- [ ] **Step 4: Update existing `New(...)` call sites in the action test**

In `action/action_test.go`, add `, nil` as the final argument to each `action.New(...)` call at lines 58, 88, 122, 155. Each currently ends with the `maxBackoff` duration argument; append `, nil`.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./action/ -v`
Expected: PASS (existing tests + TestRenderArgsUsesCustomFunc).

- [ ] **Step 6: Commit**

```bash
git add action/action.go action/action_test.go
git commit -m "feat(action): render templates with custom functions"
```

---

## Task 8: Wire funcs into the bot entry point

**Files:**
- Modify: `bot/run.go:23,33,47,60-95,76,81`
- Test: `bot/run_test.go` (create)

- [ ] **Step 1: Write the failing test**

Create `bot/run_test.go`:

```go
package bot

import "testing"

func TestWithFuncCollects(t *testing.T) {
	var o options
	WithFunc("double", func(n int) int { return n * 2 })(&o)
	WithFunc("triple", func(n int) int { return n * 3 })(&o)
	if len(o.funcs) != 2 {
		t.Fatalf("want 2 funcs, got %d", len(o.funcs))
	}
	if _, ok := o.funcs["double"]; !ok {
		t.Fatal("double not registered")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./bot/ -run TestWithFuncCollects -v`
Expected: FAIL — `options` and `WithFunc` undefined.

- [ ] **Step 3: Add options + thread funcs through the pipeline build**

In `bot/run.go`:

Add to the import block:
```go
	"github.com/joeyciechanowicz/eve-bot/internal/funcs"
```

Add the option type and constructor (after the imports, before `Run`):
```go
// Option configures a bot run. Use WithFunc to register a Go function callable
// from `when:` expressions and templated action args.
type Option func(*options)

type options struct {
	funcs map[string]any
}

// WithFunc registers a Go function under name. fn must return either (T) or
// (T, error) so it works in both expr-lang and text/template.
func WithFunc(name string, fn any) Option {
	return func(o *options) {
		if o.funcs == nil {
			o.funcs = map[string]any{}
		}
		o.funcs[name] = fn
	}
}
```

Change `Run` (line 23) to accept options and forward them:
```go
func Run(ctx context.Context, cfgPath string, opts ...Option) error {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return RunConfig(ctx, cfg, opts...)
}
```

Change `RunConfig` (line 33) to collect options and pass funcs into `buildPipeline`:
```go
func RunConfig(ctx context.Context, cfg *Config, opts ...Option) error {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	if cfg.Debug {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()
	go st.RunJanitor(ctx, cfg.Store.JanitorInterval, cfg.Store.ActionHistoryTTL)

	httpClient := &http.Client{Timeout: 15 * time.Second}

	p, err := buildPipeline(cfg, st, httpClient, o.funcs)
	if err != nil {
		return fmt.Errorf("build pipeline: %w", err)
	}
	slog.Info("eve-bot starting", "sources", len(p.Sources), "rules", len(cfg.Rules.Rules))

	if err := p.Run(ctx); err != nil {
		return err
	}
	slog.Info("eve-bot stopped")
	return nil
}
```

Change `buildPipeline` (line 60) to compile the function set and pass it to `rules.Compile` and `action.New`:
```go
func buildPipeline(cfg *Config, st *store.Store, hc *http.Client, goFuncs map[string]any) (*pipeline.Pipeline, error) {
	fnSet, err := funcs.Compile(goFuncs, cfg.Functions)
	if err != nil {
		return nil, fmt.Errorf("functions: %w", err)
	}

	handlers := action.BuildHandlers(action.Deps{
		HTTPClient: hc,
		FactWriter: st,
	})

	srcDeps := source.Deps{HTTPClient: hc, Checkpointer: st}
	srcs := make([]source.Source, 0, len(cfg.Sources))
	for _, sc := range cfg.Sources {
		s, err := source.Build(sc.Type, sc.Name, sc.Params, srcDeps)
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", sc.Name, err)
		}
		srcs = append(srcs, s)
	}

	ruleSet, err := rules.Compile(cfg.Rules.Mode, cfg.Rules.Rules, fnSet)
	if err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}

	disp := action.New(handlers, st, cfg.Retry.MaxRetries, cfg.Retry.BaseBackoff, cfg.Retry.MaxBackoff, fnSet)

	enrichers := enrich.Chain{
		names.New(hc, st, cfg.Enrich.Names.CacheTTL, cfg.Enrich.Names.BaseURL),
	}

	return &pipeline.Pipeline{
		Sources:    srcs,
		Enrichers:  enrichers,
		Rules:      ruleSet,
		Dispatcher: disp,
		Facts:      st,
		BufferSize: cfg.BufferSize,
	}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./bot/ -v`
Expected: PASS (TestWithFuncCollects, TestLoadConfigParsesFunctions).

- [ ] **Step 5: Verify the whole module still builds**

Run: `go build ./...`
Expected: no output (success). If `cmd/rule-check` fails to build here, that is expected — it is fixed in Task 9; you may run `go build ./bot/... ./action/... ./internal/...` to scope the check.

- [ ] **Step 6: Commit**

```bash
git add bot/run.go bot/run_test.go
git commit -m "feat(bot): WithFunc option and pipeline wiring for custom functions"
```

---

## Task 9: Make `cmd/rule-check` functions-aware

**Files:**
- Modify: `cmd/rule-check/main.go:36-56,75-77,239-249,276-292`
- Test: manual (CLI tool)

- [ ] **Step 1: Add a `--functions` flag and compile a set**

In `cmd/rule-check/main.go`:

Add to the import block:
```go
	"github.com/joeyciechanowicz/eve-bot/internal/funcs"
```

Add the flag in `main` (inside the `flag.String` block around line 37-39):
```go
		funcsPath = flag.String("functions", "", "optional path to a YAML file with a top-level `functions:` block")
```

After `flag.Parse()` and the required-args check, load + compile the functions (replace the existing `rules.Compile` block at lines 49-56):
```go
	rule, err := loadRule(*rulePath)
	if err != nil {
		fatal("load rule: %v", err)
	}

	fnSet, err := loadFuncs(*funcsPath)
	if err != nil {
		fatal("load functions: %v", err)
	}

	set, err := rules.Compile(rules.ModeMultiMatch, []rules.Rule{rule}, fnSet)
	if err != nil {
		fatal("%v", err)
	}
```

Add the `loadFuncs` helper (near `loadRule`):
```go
// loadFuncs compiles the `functions:` block from path, or returns nil when
// path is empty. Only YAML funcs are available to rule-check (no Go funcs).
func loadFuncs(path string) (*funcs.Set, error) {
	if path == "" {
		return funcs.Compile(nil, nil)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Functions map[string]string `yaml:"functions"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return funcs.Compile(nil, doc.Functions)
}
```

- [ ] **Step 2: Pass the set into `printExplain` and bind it in the explain env**

Update the `printExplain` call in `main` (line 75-77):
```go
	if *explain {
		printExplain(rule.When, ev, store, fnSet)
	}
```

Update `printExplain`'s signature and bind funcs so custom-func identifiers don't show as `<undefined>` (lines 225-264). Change the signature and the `buildEnv` call, and extend the skip set to include the set's function names:
```go
func printExplain(when string, ev *event.Event, facts rules.FactStore, fnSet *funcs.Set) {
	if when == "" {
		when = "true"
	}
	tree, err := parser.Parse(when)
	if err != nil {
		fmt.Fprintf(os.Stderr, "explain: parse error: %v\n", err)
		return
	}
	idents := map[string]struct{}{}
	v := &identCollector{idents: idents}
	node := tree.Node
	ast.Walk(&node, v)

	env := buildEnv(ev, facts, fnSet)
	skip := map[string]bool{"fact": true, "fact_exists": true, "fact_count": true, "now": true}
	for name := range fnSet.Names() {
		skip[name] = true
	}

	names := make([]string, 0, len(idents))
	for n := range idents {
		if skip[n] {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Println("explain:")
	for _, n := range names {
		val, present := env[n]
		switch {
		case !present:
			fmt.Printf("  %-22s <undefined>  (typo? expression sees nil)\n", n)
		case val == nil:
			fmt.Printf("  %-22s nil\n", n)
		default:
			fmt.Printf("  %-22s %s\n", n, summarize(val))
		}
	}
	fmt.Println()
}
```

Update the local `buildEnv` mirror (lines 278-292) to bind the set:
```go
func buildEnv(ev *event.Event, facts rules.FactStore, fnSet *funcs.Set) map[string]any {
	env := make(map[string]any, len(ev.Fields)+8)
	maps.Copy(env, ev.Fields)
	env["event_id"] = ev.ID
	env["event_source"] = ev.Source
	env["event_type"] = ev.Type
	env["occurred_at"] = ev.OccurredAt
	env["now"] = time.Now().UTC()
	if facts != nil {
		env["fact"] = facts.GetAny
		env["fact_exists"] = facts.Exists
		env["fact_count"] = facts.RangeCount
	}
	fnSet.BindExprEnv(env)
	return env
}
```

- [ ] **Step 3: Add the `Names()` accessor to the funcs package**

In `internal/funcs/funcs.go`, add:
```go
// Names returns the set of all function names (Go and YAML). Useful for tools
// that need to distinguish function identifiers from field references.
func (s *Set) Names() map[string]struct{} {
	out := map[string]struct{}{}
	if s == nil {
		return out
	}
	for name := range s.goFuncs {
		out[name] = struct{}{}
	}
	for _, yf := range s.yamlFuncs {
		out[yf.name] = struct{}{}
	}
	return out
}
```

Add a quick unit test in `internal/funcs/funcs_test.go`:
```go
func TestNames(t *testing.T) {
	s, err := Compile(
		map[string]any{"g": func() bool { return true }},
		map[string]string{"y(a)": "a > 0"},
	)
	if err != nil {
		t.Fatal(err)
	}
	names := s.Names()
	if _, ok := names["g"]; !ok {
		t.Fatal("missing g")
	}
	if _, ok := names["y"]; !ok {
		t.Fatal("missing y")
	}
}
```

- [ ] **Step 4: Build and smoke-test the tool**

Run: `go build ./...`
Expected: success (whole module now builds).

Run: `go test ./internal/funcs/ -run TestNames -v`
Expected: PASS

Create `/tmp/fns.yaml`:
```yaml
functions:
  'is_expensive(threshold)': 'total_value > threshold'
```
Create `/tmp/rule.yaml`:
```yaml
name: expensive
when: 'is_expensive(1000)'
```
Create `/tmp/ev.json`:
```json
{ "total_value": 5000 }
```
Run: `go run ./cmd/rule-check --rule /tmp/rule.yaml --event /tmp/ev.json --functions /tmp/fns.yaml --explain`
Expected: prints an `explain:` block (without `is_expensive` listed as undefined) and ends with `MATCH  expensive` (exit code 0).

- [ ] **Step 5: Commit**

```bash
git add cmd/rule-check/main.go internal/funcs/funcs.go internal/funcs/funcs_test.go
git commit -m "feat(rule-check): load and bind custom functions"
```

---

## Task 10: End-to-end integration test

**Files:**
- Create: `internal/funcs/integration_test.go`

- [ ] **Step 1: Write the test**

Create `internal/funcs/integration_test.go`. It exercises one shared `Set` through both the rules engine and the action render path, proving a Go func and a YAML func work in both contexts:

```go
package funcs_test

import (
	"testing"

	"github.com/joeyciechanowicz/eve-bot/event"
	"github.com/joeyciechanowicz/eve-bot/internal/funcs"
	"github.com/joeyciechanowicz/eve-bot/internal/rules"
)

func TestCustomFuncsInRulesAndTemplates(t *testing.T) {
	set, err := funcs.Compile(
		map[string]any{
			"upper": func(s string) string {
				out := []rune(s)
				for i, r := range out {
					if r >= 'a' && r <= 'z' {
						out[i] = r - 32
					}
				}
				return string(out)
			},
		},
		map[string]string{
			"is_expensive(threshold)": "total_value > threshold",
		},
	)
	if err != nil {
		t.Fatalf("compile set: %v", err)
	}

	// --- In a when: expression ---
	rs, err := rules.Compile(rules.ModeMultiMatch, []rules.Rule{
		{Name: "expensive", Enabled: true, When: "is_expensive(1000) && upper(name) == \"JITA\""},
	}, set)
	if err != nil {
		t.Fatalf("compile rules: %v", err)
	}
	ev := &event.Event{
		ID: "k1", Source: "zkill", Type: "killmail",
		Fields: map[string]any{"total_value": 5000, "name": "jita"},
	}
	matches := rs.Evaluate(ev, nil)
	if len(matches) != 1 {
		t.Fatalf("expected 1 rule match, got %d", len(matches))
	}

	// --- In a template (via the FuncMap directly) ---
	fm := set.TemplateFuncMap(map[string]any{"total_value": 5000, "name": "jita"})
	if _, ok := fm["upper"]; !ok {
		t.Fatal("upper not exposed to templates")
	}
	if _, ok := fm["is_expensive"]; !ok {
		t.Fatal("is_expensive not exposed to templates")
	}
}
```

- [ ] **Step 2: Run the test**

Run: `go test ./internal/funcs/ -run TestCustomFuncsInRulesAndTemplates -v`
Expected: PASS

- [ ] **Step 3: Run the whole suite**

Run: `go test ./...`
Expected: PASS across all packages.

- [ ] **Step 4: Commit**

```bash
git add internal/funcs/integration_test.go
git commit -m "test(funcs): end-to-end custom funcs in rules and templates"
```

---

## Task 11: Documentation

**Files:**
- Modify: `RULES.md` (the rules reference doc referenced in earlier exploration)
- Modify: `config.yaml` (add a commented example)

- [ ] **Step 1: Document the `functions:` block in RULES.md**

Open `RULES.md`, find the section that lists built-in expression helpers (`fact`, `now`, etc.), and add a new subsection after it:

```markdown
## Custom functions

Declare reusable functions in a top-level `functions:` block. The key is a
signature; the value is an expr-lang body. Bodies may use builtins, the event
fields, other custom functions, and any Go functions the host program
registered via `bot.WithFunc`.

```yaml
functions:
  'is_expensive(threshold)': 'zkb.total_value > threshold'
  'near_jita(system, jumps)': 'distance(system, 30000142) <= jumps'
```

Use them in any `when:` clause or templated action arg:

```yaml
rules:
  - name: pricey
    when: 'is_expensive(1e9)'
    actions:
      - type: console
        args:
          msg: '{{ if is_expensive 1e9 }}BIG{{ end }} kill'
```

Cycles between functions are rejected at startup; functions are not recursive.
Go functions are registered in code:

```go
bot.Run(ctx, "config.yaml",
    bot.WithFunc("distance", func(a, b int64) int { /* ... */ }),
)
```
```

> If `RULES.md` does not exist at the repo root, search for the rules reference doc (`grep -ril "fact_count" *.md docs/`) and add the section there instead.

- [ ] **Step 2: Add a commented example to config.yaml**

In `config.yaml`, add near the top (after the `enrich:` block) a commented-out example so operators discover the feature:

```yaml
# functions:
#   'is_expensive(threshold)': 'zkb.total_value > threshold'
```

- [ ] **Step 3: Commit**

```bash
git add RULES.md config.yaml
git commit -m "docs: document custom functions block"
```

---

## Self-Review Notes

- **Spec coverage:** config surface (Task 1), `internal/funcs` package with Compile/validation/cycle-detection/BindExprEnv/TemplateFuncMap (Tasks 2-5), rules wiring (Task 6), action wiring (Task 7), `WithFunc` Go API (Task 8), rule-check (Task 9), unit + integration tests (Tasks 2-5, 6, 7, 10), docs (Task 11). All spec sections map to a task.
- **Type consistency:** `funcs.Compile(goFuncs map[string]any, yamlSrc map[string]string) (*Set, error)`, `(*Set).BindExprEnv(map[string]any)`, `(*Set).TemplateFuncMap(map[string]any) template.FuncMap`, and `(*Set).Names() map[string]struct{}` are used identically across rules, action, bot, and rule-check. `rules.Compile(mode, raw, *funcs.Set)` and `action.New(..., *funcs.Set)` signatures match every call site updated in Tasks 6-9.
- **Nil-safety:** `BindExprEnv`, `TemplateFuncMap`, and `Names` all guard `s == nil`; `buildPipeline` always passes a non-nil compiled set, while tests pass `nil` to confirm the guards.
```
