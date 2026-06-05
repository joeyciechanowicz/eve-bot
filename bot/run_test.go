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
