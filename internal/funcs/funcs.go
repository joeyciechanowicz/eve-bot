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
	bodyByName := map[string]string{}
	for key, body := range yamlSrc {
		name, _, err := parseSignature(key)
		if err != nil {
			return err
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
