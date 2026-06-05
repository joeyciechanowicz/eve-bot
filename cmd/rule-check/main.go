// Command rule-check compiles a single rule, evaluates it against an event
// fixture, and reports match / no-match. Used by rule authors to validate a
// `when:` clause before pasting it into config.yaml.
//
// Exit codes:
//
//	0 — rule matched
//	1 — rule did not match
//	2 — compile or runtime error (treat as a bug to fix, not no-match)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
	"gopkg.in/yaml.v3"

	"github.com/joeyciechanowicz/eve-bot/event"
	"github.com/joeyciechanowicz/eve-bot/internal/funcs"
	"github.com/joeyciechanowicz/eve-bot/internal/rules"
)

type factFlags []string

func (f *factFlags) String() string     { return strings.Join(*f, ",") }
func (f *factFlags) Set(s string) error { *f = append(*f, s); return nil }

func main() {
	var (
		rulePath  = flag.String("rule", "", "path to a YAML file containing a single rule")
		eventPath = flag.String("event", "", "path to a JSON fixture (Event.Fields shape, see testdata/killmails)")
		explain   = flag.Bool("explain", false, "print each identifier referenced in `when:` with its resolved value")
		funcsPath = flag.String("functions", "", "optional path to a YAML file with a top-level `functions:` block")
		facts     factFlags
	)
	flag.Var(&facts, "fact", "seed a fact: --fact scope:key=<json>; repeatable")
	flag.Parse()

	if *rulePath == "" || *eventPath == "" {
		fatal("--rule and --event are required")
	}

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

	fields, err := loadFields(*eventPath)
	if err != nil {
		fatal("load event: %v", err)
	}
	ev := &event.Event{
		ID:         fmt.Sprintf("fixture:%s", *eventPath),
		Source:     "zkill",
		Type:       "killmail",
		OccurredAt: occurredAt(fields),
		Fields:     fields,
	}

	store, err := buildFacts(facts)
	if err != nil {
		fatal("parse --fact: %v", err)
	}

	if *explain {
		printExplain(rule.When, ev, store, fnSet)
	}

	matches := set.Evaluate(ev, store)
	if len(matches) > 0 {
		fmt.Printf("MATCH  %s\n", rule.Name)
		os.Exit(0)
	}
	fmt.Printf("no-match  %s\n", rule.Name)
	os.Exit(1)
}

func loadRule(path string) (rules.Rule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return rules.Rule{}, err
	}
	// Accept either a bare rule object or a single-element list.
	var single rules.Rule
	if err := yaml.Unmarshal(b, &single); err == nil && single.Name != "" {
		single.Enabled = true
		return single, nil
	}
	var list []rules.Rule
	if err := yaml.Unmarshal(b, &list); err != nil {
		return rules.Rule{}, err
	}
	if len(list) != 1 {
		return rules.Rule{}, fmt.Errorf("expected exactly one rule, got %d", len(list))
	}
	list[0].Enabled = true
	return list[0], nil
}

// loadFuncs compiles the `functions:` block from path, or returns an empty set
// when path is empty. Only YAML funcs are available to rule-check (no Go funcs).
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

func loadFields(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	return convertNumbers(raw).(map[string]any), nil
}

// convertNumbers walks a json.Number-typed tree and produces int64 for
// integral values, float64 otherwise. Mirrors what internal/source/zkill
// produces from the wire format so rule expressions see the same types in
// fixtures as they do in production.
func convertNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			x[k] = convertNumbers(vv)
		}
		return x
	case []any:
		for i, vv := range x {
			x[i] = convertNumbers(vv)
		}
		return x
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") {
			if n, err := x.Int64(); err == nil {
				return n
			}
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return s
	default:
		return v
	}
}

func occurredAt(fields map[string]any) time.Time {
	if s, ok := fields["killmail_time"].(string); ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// memFacts is an in-memory FactStore seeded from --fact flags.
type memFacts struct {
	data map[string]map[string]any // scope -> key -> value
}

func (m *memFacts) GetAny(scope, key string) any {
	if m == nil {
		return nil
	}
	return m.data[scope][key]
}

func (m *memFacts) Exists(scope, key string) bool {
	if m == nil {
		return false
	}
	_, ok := m.data[scope][key]
	return ok
}

func (m *memFacts) RangeCount(scope, prefix string) int {
	if m == nil {
		return 0
	}
	n := 0
	for k := range m.data[scope] {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

func buildFacts(flags factFlags) (*memFacts, error) {
	m := &memFacts{data: map[string]map[string]any{}}
	for _, raw := range flags {
		// Format: scope:key=<json>
		head, payload, ok := strings.Cut(raw, "=")
		if !ok {
			return nil, fmt.Errorf("missing '=' in %q (want scope:key=<json>)", raw)
		}
		scope, key, ok := strings.Cut(head, ":")
		if !ok {
			return nil, fmt.Errorf("missing ':' in %q (want scope:key=<json>)", raw)
		}
		var v any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			return nil, fmt.Errorf("decode value for %s:%s: %w", scope, key, err)
		}
		if m.data[scope] == nil {
			m.data[scope] = map[string]any{}
		}
		m.data[scope][key] = v
	}
	return m, nil
}

// printExplain shows each top-level identifier from the `when:` expression
// and the value it resolved to in the event's environment. Spotting `nil`
// here is the fastest way to catch typo'd field names.
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

type identCollector struct {
	idents map[string]struct{}
}

func (c *identCollector) Visit(node *ast.Node) {
	if id, ok := (*node).(*ast.IdentifierNode); ok {
		c.idents[id.Value] = struct{}{}
	}
}

// buildEnv mirrors rules.buildEnv (which is unexported). Kept in lockstep:
// if the rules package adds a reserved identifier, add it here too.
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

func summarize(v any) string {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 6 {
			keys = append(keys[:6], "...")
		}
		return fmt.Sprintf("object{%s}", strings.Join(keys, ", "))
	case []any:
		return fmt.Sprintf("array[%d]", len(x))
	case string:
		if len(x) > 60 {
			return fmt.Sprintf("%q (truncated)", x[:60])
		}
		return fmt.Sprintf("%q", x)
	case time.Time:
		return x.Format(time.RFC3339)
	default:
		return fmt.Sprintf("%v (%T)", v, v)
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(2)
}
