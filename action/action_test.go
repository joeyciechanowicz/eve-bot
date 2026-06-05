package action_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joeyciechanowicz/eve-bot/action"
	"github.com/joeyciechanowicz/eve-bot/event"
	"github.com/joeyciechanowicz/eve-bot/internal/funcs"
	"github.com/joeyciechanowicz/eve-bot/internal/rules"
)

type fakeIdem struct {
	done     map[string]bool
	recorded []string
}

func (f *fakeIdem) ActionDone(id, fp string) bool { return f.done[id+"|"+fp] }
func (f *fakeIdem) RecordAction(id, fp string) error {
	f.recorded = append(f.recorded, id+"|"+fp)
	return nil
}

type fakeHandler struct {
	calls atomic.Int32
	err   error
	args  []map[string]any
}

func (h *fakeHandler) Execute(_ context.Context, _ *event.Event, args map[string]any) error {
	h.calls.Add(1)
	h.args = append(h.args, args)
	return h.err
}

func sampleEvent() *event.Event {
	return &event.Event{
		ID:     "zkill:42",
		Source: "zkill",
		Type:   "killmail",
		Fields: map[string]any{
			"killmail_id": int64(42),
			"attackers": []any{
				map[string]any{"character_id": int64(111)},
				map[string]any{"character_id": int64(222)},
			},
		},
	}
}

func TestForEachRunsOncePerItem(t *testing.T) {
	h := &fakeHandler{}
	d := action.New(
		map[string]action.Handler{"test": h},
		&fakeIdem{done: map[string]bool{}},
		0, time.Millisecond, time.Millisecond, nil,
	)
	matches := []rules.Match{{
		Rule: &rules.Rule{Name: "r"},
		Actions: []rules.ActionConfig{{
			Type: "test",
			For:  "attackers",
			Args: map[string]any{"key": "{{ .item.character_id }}"},
		}},
	}}
	d.Dispatch(context.Background(), sampleEvent(), matches)

	if h.calls.Load() != 2 {
		t.Errorf("calls: %d, want 2", h.calls.Load())
	}
	got := []string{h.args[0]["key"].(string), h.args[1]["key"].(string)}
	want := []string{"111", "222"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d].key: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIdempotencySkipsSecondRun(t *testing.T) {
	h := &fakeHandler{}
	idem := &fakeIdem{done: map[string]bool{}}
	d := action.New(
		map[string]action.Handler{"test": h},
		idem, 0, time.Millisecond, time.Millisecond, nil,
	)
	matches := []rules.Match{{
		Rule:    &rules.Rule{Name: "r"},
		Actions: []rules.ActionConfig{{Type: "test", Args: map[string]any{}}},
	}}
	d.Dispatch(context.Background(), sampleEvent(), matches)
	if h.calls.Load() != 1 {
		t.Fatalf("first call: got %d, want 1", h.calls.Load())
	}

	// Mark the fingerprint done; second dispatch should skip.
	for _, fp := range idem.recorded {
		idem.done[fp] = true
	}
	d.Dispatch(context.Background(), sampleEvent(), matches)
	if h.calls.Load() != 1 {
		t.Errorf("second call: got %d, want 1 (skipped)", h.calls.Load())
	}
}

func TestRetryOnError(t *testing.T) {
	calls := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := action.New(
		map[string]action.Handler{"webhook": action.Webhook{Client: srv.Client()}},
		nil, 3, time.Millisecond, 5*time.Millisecond, nil,
	)
	matches := []rules.Match{{
		Rule: &rules.Rule{Name: "r"},
		Actions: []rules.ActionConfig{{
			Type: "webhook",
			Args: map[string]any{"url": srv.URL},
		}},
	}}
	d.Dispatch(context.Background(), sampleEvent(), matches)

	if calls.Load() != 3 {
		t.Errorf("webhook hit %d times, want 3", calls.Load())
	}
	if d.Counters.Success != 1 {
		t.Errorf("success: %d, want 1", d.Counters.Success)
	}
	if d.Counters.Retry == 0 {
		t.Error("expected retry counter > 0")
	}
}

func TestWebhookSendsBody(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(204)
	}))
	defer srv.Close()

	d := action.New(
		map[string]action.Handler{"webhook": action.Webhook{Client: srv.Client()}},
		nil, 0, time.Millisecond, time.Millisecond, nil,
	)
	matches := []rules.Match{{
		Rule: &rules.Rule{Name: "r"},
		Actions: []rules.ActionConfig{{
			Type: "webhook",
			Args: map[string]any{"url": srv.URL},
		}},
	}}
	d.Dispatch(context.Background(), sampleEvent(), matches)

	if got["id"] != "zkill:42" {
		t.Errorf("webhook body: %+v", got)
	}
}

// TestDispatchUsesCustomFunc exercises the custom-func render path through the
// exported Dispatch API (action_test is black-box, so renderArgs is not
// reachable). A capturing handler records the rendered args.
func TestDispatchUsesCustomFunc(t *testing.T) {
	fns, err := funcs.Compile(map[string]any{
		"upper": func(s string) string { return strings.ToUpper(s) },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHandler{}
	d := action.New(
		map[string]action.Handler{"test": h},
		nil, 0, time.Millisecond, time.Millisecond, fns,
	)
	ev := &event.Event{
		ID: "x", Source: "zkill", Type: "killmail",
		Fields: map[string]any{"name": "jita"},
	}
	matches := []rules.Match{{
		Rule: &rules.Rule{Name: "r"},
		Actions: []rules.ActionConfig{{
			Type: "test",
			Args: map[string]any{"text": "{{ upper .name }}"},
		}},
	}}
	d.Dispatch(context.Background(), ev, matches)

	if h.calls.Load() != 1 {
		t.Fatalf("calls: %d, want 1", h.calls.Load())
	}
	if h.args[0]["text"] != "JITA" {
		t.Fatalf("text = %v, want JITA", h.args[0]["text"])
	}
}
