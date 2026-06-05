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
