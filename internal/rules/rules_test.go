package rules_test

import (
	"testing"

	"github.com/joeyciechanowicz/eve-bot/event"
	"github.com/joeyciechanowicz/eve-bot/internal/funcs"
	"github.com/joeyciechanowicz/eve-bot/internal/rules"
)

type fakeFacts struct {
	data   map[string]any
	counts map[string]int
}

func (f *fakeFacts) GetAny(scope, key string) any { return f.data[scope+":"+key] }
func (f *fakeFacts) Exists(scope, key string) bool { _, ok := f.data[scope+":"+key]; return ok }
func (f *fakeFacts) RangeCount(scope, prefix string) int { return f.counts[scope+":"+prefix] }

func sampleEvent(totalValue float64) *event.Event {
	return &event.Event{
		ID:     "zkill:1",
		Source: "zkill",
		Type:   "killmail",
		Fields: map[string]any{
			"zkb": map[string]any{
				"total_value": totalValue,
				"npc":         false,
			},
			"has_capital": false,
			"attackers": []any{
				map[string]any{"character_id": int64(111)},
				map[string]any{"character_id": int64(222)},
			},
		},
	}
}

func TestCompileRejectsBadExpression(t *testing.T) {
	_, err := rules.Compile(rules.ModeFirstMatch, []rules.Rule{
		{Name: "bad", Enabled: true, When: "zkb.total_value >", Actions: nil},
	}, nil)
	if err == nil {
		t.Fatal("expected compile error")
	}
}

func TestFirstMatchStops(t *testing.T) {
	rs, err := rules.Compile(rules.ModeFirstMatch, []rules.Rule{
		{Name: "a", Enabled: true, Priority: 1, When: "zkb.total_value > 0"},
		{Name: "b", Enabled: true, Priority: 2, When: "true"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := rs.Evaluate(sampleEvent(1), nil)
	if len(m) != 1 || m[0].Rule.Name != "a" {
		t.Errorf("matches: %+v", m)
	}
}

func TestContinueFlag(t *testing.T) {
	rs, err := rules.Compile(rules.ModeFirstMatch, []rules.Rule{
		{Name: "writer", Enabled: true, Priority: 1, Continue: true, When: "true"},
		{Name: "reader", Enabled: true, Priority: 2, When: "zkb.total_value > 0"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := rs.Evaluate(sampleEvent(1), nil)
	if len(m) != 2 {
		t.Errorf("expected 2 matches, got %d (%+v)", len(m), m)
	}
}

func TestFactHelperInExpression(t *testing.T) {
	facts := &fakeFacts{data: map[string]any{
		"kill_by_char:111": map[string]any{"count": float64(10)},
	}}
	rs, err := rules.Compile(rules.ModeMultiMatch, []rules.Rule{
		{Name: "repeat", Enabled: true, Priority: 1, When: `
			any(attackers, {
				let f = fact("kill_by_char", string(.character_id));
				f != nil && f.count >= 5
			})`},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := rs.Evaluate(sampleEvent(0), facts)
	if len(m) != 1 {
		t.Errorf("expected match, got %d", len(m))
	}
}

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
