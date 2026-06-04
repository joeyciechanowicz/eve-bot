package names

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/joeyciechanowicz/eve-bot/event"
)

// memCache is an in-memory NameCache used in tests.
type memCache struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemCache() *memCache { return &memCache{data: map[string][]byte{}} }

func (m *memCache) Get(scope, key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[scope+"|"+key]
	if !ok {
		return nil, false
	}
	return v, true
}

func (m *memCache) Put(scope, key string, value any, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	m.data[scope+"|"+key] = raw
	return nil
}

// makeEvent constructs an event mirroring what zkill normalize produces.
func makeEvent() *event.Event {
	attacker := map[string]any{
		"character_id":   int64(1001),
		"corporation_id": int64(2001),
		"alliance_id":    int64(3001),
		"final_blow":     true,
	}
	return &event.Event{
		ID:     "zkill:test",
		Source: "zkill",
		Type:   "killmail",
		Fields: map[string]any{
			"victim": map[string]any{
				"character_id":   int64(1000),
				"corporation_id": int64(2000),
				"alliance_id":    int64(3000),
			},
			"attackers":  []any{attacker},
			"final_blow": attacker,
		},
	}
}

// esiStub serves /v3/universe/names/ from a fixed table, counting calls.
type esiStub struct {
	calls int
	names map[int64]esiName
	mu    sync.Mutex
}

func (s *esiStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls++
		s.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		var ids []int64
		if err := json.Unmarshal(raw, &ids); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		out := make([]esiName, 0, len(ids))
		for _, id := range ids {
			if n, ok := s.names[id]; ok {
				out = append(out, n)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

func TestEnrich_FetchesAndCaches(t *testing.T) {
	stub := &esiStub{names: map[int64]esiName{
		1000: {ID: 1000, Name: "Victim Pilot", Category: "character"},
		2000: {ID: 2000, Name: "Victim Corp", Category: "corporation"},
		3000: {ID: 3000, Name: "Victim Alliance", Category: "alliance"},
		1001: {ID: 1001, Name: "Attacker Pilot", Category: "character"},
		2001: {ID: 2001, Name: "Attacker Corp", Category: "corporation"},
		3001: {ID: 3001, Name: "Attacker Alliance", Category: "alliance"},
	}}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	cache := newMemCache()
	e := New(srv.Client(), cache, time.Hour, srv.URL)

	ev := makeEvent()
	if err := e.Enrich(context.Background(), ev); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	v := ev.Fields["victim"].(map[string]any)
	if v["character_name"] != "Victim Pilot" {
		t.Errorf("victim character_name = %v", v["character_name"])
	}
	if v["corporation_name"] != "Victim Corp" {
		t.Errorf("victim corporation_name = %v", v["corporation_name"])
	}
	if v["alliance_name"] != "Victim Alliance" {
		t.Errorf("victim alliance_name = %v", v["alliance_name"])
	}

	a := ev.Fields["attackers"].([]any)[0].(map[string]any)
	if a["character_name"] != "Attacker Pilot" {
		t.Errorf("attacker character_name = %v", a["character_name"])
	}
	if a["corporation_name"] != "Attacker Corp" {
		t.Errorf("attacker corporation_name = %v", a["corporation_name"])
	}
	if a["alliance_name"] != "Attacker Alliance" {
		t.Errorf("attacker alliance_name = %v", a["alliance_name"])
	}

	fb := ev.Fields["final_blow"].(map[string]any)
	if fb["character_name"] != "Attacker Pilot" {
		t.Errorf("final_blow shares map: character_name = %v", fb["character_name"])
	}

	if stub.calls != 1 {
		t.Errorf("expected 1 ESI call, got %d", stub.calls)
	}

	// Second enrichment on a fresh event should hit cache only.
	ev2 := makeEvent()
	if err := e.Enrich(context.Background(), ev2); err != nil {
		t.Fatalf("enrich2: %v", err)
	}
	if stub.calls != 1 {
		t.Errorf("expected cache hits only, got %d calls", stub.calls)
	}
}

func TestEnrich_DedupesAcrossActors(t *testing.T) {
	var seen [][]int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var ids []int64
		_ = json.Unmarshal(raw, &ids)
		slices.Sort(ids)
		seen = append(seen, ids)
		out := []esiName{}
		for _, id := range ids {
			out = append(out, esiName{ID: id, Name: "n", Category: "character"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	e := New(srv.Client(), nil, time.Hour, srv.URL)
	// Two attackers in the same corp/alliance; corp id repeats.
	attacker1 := map[string]any{"character_id": int64(11), "corporation_id": int64(20), "alliance_id": int64(30)}
	attacker2 := map[string]any{"character_id": int64(12), "corporation_id": int64(20), "alliance_id": int64(30)}
	ev := &event.Event{ID: "zkill:dedup", Fields: map[string]any{
		"victim":    map[string]any{"character_id": int64(11), "corporation_id": int64(20)},
		"attackers": []any{attacker1, attacker2},
	}}
	if err := e.Enrich(context.Background(), ev); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("expected 1 ESI call, got %d", len(seen))
	}
	// Should have requested: 11, 12, 20, 30 — each once.
	want := []int64{11, 12, 20, 30}
	if len(seen[0]) != len(want) {
		t.Fatalf("got ids %v, want %v", seen[0], want)
	}
	for i, id := range want {
		if seen[0][i] != id {
			t.Errorf("ids[%d]=%d, want %d (full: %v)", i, seen[0][i], id, seen[0])
		}
	}
}

func TestEnrich_SkipsZeroIDs(t *testing.T) {
	var requested []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &requested)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]esiName{})
	}))
	defer srv.Close()

	e := New(srv.Client(), nil, time.Hour, srv.URL)
	ev := &event.Event{ID: "zkill:zeros", Fields: map[string]any{
		"victim": map[string]any{
			"character_id":   int64(0), // NPC
			"corporation_id": int64(2000),
			"alliance_id":    int64(0), // no alliance
		},
	}}
	if err := e.Enrich(context.Background(), ev); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if len(requested) != 1 || requested[0] != 2000 {
		t.Errorf("requested = %v, want [2000]", requested)
	}
	v := ev.Fields["victim"].(map[string]any)
	if _, ok := v["character_name"]; ok {
		t.Errorf("character_name should not be set for zero id")
	}
	if _, ok := v["alliance_name"]; ok {
		t.Errorf("alliance_name should not be set for zero id")
	}
}

func TestEnrich_ESIFailureIsNonFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := New(srv.Client(), nil, time.Hour, srv.URL)
	ev := makeEvent()
	if err := e.Enrich(context.Background(), ev); err != nil {
		t.Fatalf("enrich should swallow ESI errors, got %v", err)
	}
	v := ev.Fields["victim"].(map[string]any)
	if _, ok := v["character_name"]; ok {
		t.Errorf("no names should be attached on ESI failure")
	}
}

func TestEnrich_AllCacheHitsSkipESI(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cache := newMemCache()
	// Pre-populate cache for every id in the event.
	for id, name := range map[int64]string{
		1000: "VC", 2000: "VCorp", 3000: "VA",
		1001: "AC", 2001: "ACorp", 3001: "AA",
	} {
		if err := cache.Put(cacheScope, strconv.FormatInt(id, 10), cachedName{Name: name, Category: "x"}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	e := New(srv.Client(), cache, time.Hour, srv.URL)
	ev := makeEvent()
	if err := e.Enrich(context.Background(), ev); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if calls != 0 {
		t.Errorf("expected zero ESI calls on full cache hit, got %d", calls)
	}
	v := ev.Fields["victim"].(map[string]any)
	if v["character_name"] != "VC" || v["corporation_name"] != "VCorp" || v["alliance_name"] != "VA" {
		t.Errorf("victim names not attached from cache: %+v", v)
	}
}

