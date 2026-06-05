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
