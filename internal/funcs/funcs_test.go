package funcs

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/expr-lang/expr"
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
	goFns := map[string]any{"bad": func() (int, int) { return 1, 2 }}
	_, err := Compile(goFns, nil)
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("want signature error, got %v", err)
	}
}

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
			"a(x)": "b(x) + inc(x)",
			"b(x)": "x * 2",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := runExpr(t, s, "a(10)", map[string]any{})
	if got != 31 {
		t.Fatalf("a(10) = %v, want 31", got)
	}
}

func TestBindExprEnv_CloneIsolation(t *testing.T) {
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
