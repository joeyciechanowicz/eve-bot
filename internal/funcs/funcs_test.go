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
	goFns := map[string]any{"bad": func() (int, int) { return 1, 2 }}
	_, err := Compile(goFns, nil)
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("want signature error, got %v", err)
	}
}
