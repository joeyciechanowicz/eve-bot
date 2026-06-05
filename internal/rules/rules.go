// Package rules is the rule engine. Rules are YAML-declarative and the `when`
// clause is a boolean expression in expr-lang syntax, compiled once at load
// time and evaluated per event. Matched rules yield their configured actions.
package rules

import (
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"time"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/joeyciechanowicz/eve-bot/event"
	"github.com/joeyciechanowicz/eve-bot/internal/funcs"
)

// Mode controls whether evaluation stops at the first match or continues.
type Mode string

const (
	ModeFirstMatch Mode = "first-match"
	ModeMultiMatch Mode = "multi-match"
)

// ActionConfig holds the action type and optional per-rule arguments. The
// args shape is action-specific and interpreted by the action package.
type ActionConfig struct {
	Type string         `yaml:"type"`
	Args map[string]any `yaml:"args"`
	For  string         `yaml:"for"` // optional field path of an array; action runs once per item
}

// Rule is a single configured rule.
type Rule struct {
	Name     string         `yaml:"name"`
	Enabled  bool           `yaml:"enabled"`
	Priority int            `yaml:"priority"`
	When     string         `yaml:"when"`
	Continue bool           `yaml:"continue"` // in first-match mode, don't stop here
	Actions  []ActionConfig `yaml:"actions"`
	// Sources optionally restricts this rule to events whose event.Source
	// matches one of the listed configured source names. An empty list means
	// the rule applies to events from every source.
	Sources []string `yaml:"sources"`

	// program is the compiled `when` expression. Populated by Compile.
	program *vm.Program
	// sourceSet is the fast-path membership check for Sources.
	sourceSet map[string]struct{}
}

// Set is a compiled, priority-sorted collection of rules.
type Set struct {
	Mode  Mode
	Rules []Rule
	fns   *funcs.Set
}

// FactStore is the subset of internal/store.Store that rule expressions need.
type FactStore interface {
	GetAny(scope, key string) any
	Exists(scope, key string) bool
	RangeCount(scope, prefix string) int
}

// Compile validates + compiles every rule's `when` expression. Returns on the
// first error so misconfigurations fail fast at startup.
func Compile(mode Mode, raw []Rule, fns *funcs.Set) (*Set, error) {
	if mode == "" {
		mode = ModeFirstMatch
	}
	if mode != ModeFirstMatch && mode != ModeMultiMatch {
		return nil, fmt.Errorf("rules: unknown mode %q", mode)
	}

	out := make([]Rule, len(raw))
	for i, r := range raw {
		if r.Name == "" {
			return nil, fmt.Errorf("rules: rule at index %d has no name", i)
		}
		when := r.When
		if when == "" {
			when = "true"
		}
		prog, err := expr.Compile(when,
			expr.AsBool(),
			expr.AllowUndefinedVariables(),
		)
		if err != nil {
			return nil, fmt.Errorf("rules: compile %q: %w", r.Name, err)
		}
		r.program = prog
		if len(r.Sources) > 0 {
			r.sourceSet = make(map[string]struct{}, len(r.Sources))
			for _, s := range r.Sources {
				r.sourceSet[s] = struct{}{}
			}
		}
		out[i] = r
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Priority < out[j].Priority
	})
	return &Set{Mode: mode, Rules: out, fns: fns}, nil
}

// Match is a single rule match on a single event.
type Match struct {
	Rule    *Rule
	Actions []ActionConfig
}

// Evaluate runs all enabled rules against ev. In first-match mode a matched
// rule stops evaluation unless its Continue flag is set.
func (s *Set) Evaluate(ev *event.Event, facts FactStore) []Match {
	if s == nil {
		return nil
	}
	env := buildEnv(ev, facts, s.fns)

	var matches []Match
	for i := range s.Rules {
		r := &s.Rules[i]
		if !r.Enabled {
			continue
		}
		if r.sourceSet != nil {
			if _, ok := r.sourceSet[ev.Source]; !ok {
				continue
			}
		}
		v, err := expr.Run(r.program, env)
		if err != nil {
			slog.Warn("rules: eval error", "rule", r.Name, "error", err)
			continue
		}
		hit, _ := v.(bool)
		if !hit {
			continue
		}
		matches = append(matches, Match{Rule: r, Actions: r.Actions})
		if s.Mode == ModeFirstMatch && !r.Continue {
			break
		}
	}
	return matches
}

// buildEnv produces the per-event expr environment: event fields at the top
// level, plus fact helpers and event metadata. Reserved identifiers:
//   event_id, event_source, event_type, occurred_at,
//   fact, fact_exists, fact_count, now
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
	fns.BindExprEnv(env)
	return env
}
