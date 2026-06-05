// Package action executes matched rules' side effects. A Handler implements
// one action type (console, webhook, store, reply). The Dispatcher wires a
// set of handlers, applies per-iteration templating to args, retries with
// backoff, and prevents duplicate execution via the store's idempotency log.
package action

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"sort"
	"text/template"
	"time"

	"github.com/joeyciechanowicz/eve-bot/event"
	"github.com/joeyciechanowicz/eve-bot/internal/funcs"
	"github.com/joeyciechanowicz/eve-bot/internal/rules"
)

// Handler implements a single action type. args are already template-rendered
// and safe to use as-is.
type Handler interface {
	Execute(ctx context.Context, ev *event.Event, args map[string]any) error
}

// IdempotencyStore records (eventID, fingerprint) tuples to prevent duplicate
// action execution across retries and restarts.
type IdempotencyStore interface {
	ActionDone(eventID, fingerprint string) bool
	RecordAction(eventID, fingerprint string) error
}

// Counters tracks action outcomes for observability.
type Counters struct {
	Success int64
	Failure int64
	Retry   int64
	Skipped int64
}

// Dispatcher routes matches to handlers.
type Dispatcher struct {
	handlers    map[string]Handler
	maxRetries  int
	baseBackoff time.Duration
	maxBackoff  time.Duration
	idem        IdempotencyStore
	fns         *funcs.Set
	Counters    Counters
}

// New builds a dispatcher. Pass nil for idem to disable idempotency checks.
func New(handlers map[string]Handler, idem IdempotencyStore, maxRetries int, baseBackoff, maxBackoff time.Duration, fns *funcs.Set) *Dispatcher {
	return &Dispatcher{
		handlers:    handlers,
		maxRetries:  maxRetries,
		baseBackoff: baseBackoff,
		maxBackoff:  maxBackoff,
		idem:        idem,
		fns:         fns,
	}
}

// Dispatch executes every action for every match, honoring `for:` iteration,
// templating, idempotency, and retry policy.
func (d *Dispatcher) Dispatch(ctx context.Context, ev *event.Event, matches []rules.Match) {
	for _, m := range matches {
		for _, ac := range m.Actions {
			d.runAction(ctx, ev, m.Rule, ac)
		}
	}
}

func (d *Dispatcher) runAction(ctx context.Context, ev *event.Event, rule *rules.Rule, ac rules.ActionConfig) {
	handler, ok := d.handlers[ac.Type]
	if !ok {
		slog.Error("action: unknown type", "rule", rule.Name, "type", ac.Type)
		d.Counters.Failure++
		return
	}

	items := []any{nil}
	if ac.For != "" {
		v := ev.Get(ac.For)
		arr, ok := v.([]any)
		if !ok {
			slog.Warn("action: for path is not an array", "rule", rule.Name, "path", ac.For)
			return
		}
		items = arr
	}

	for idx, item := range items {
		args, err := renderArgs(ac.Args, ev, item, d.fns)
		if err != nil {
			slog.Error("action: render args", "rule", rule.Name, "type", ac.Type, "error", err)
			d.Counters.Failure++
			continue
		}

		fp := fingerprint(rule.Name, ac.Type, idx, args)
		if d.idem != nil && ev.ID != "" && d.idem.ActionDone(ev.ID, fp) {
			d.Counters.Skipped++
			continue
		}

		if err := d.execWithRetry(ctx, handler, ev, args, rule.Name, ac.Type); err != nil {
			slog.Error("action: failed after retries",
				"rule", rule.Name, "action", ac.Type, "error", err)
			d.Counters.Failure++
			continue
		}
		d.Counters.Success++
		if d.idem != nil && ev.ID != "" {
			if err := d.idem.RecordAction(ev.ID, fp); err != nil {
				slog.Warn("action: record history", "error", err)
			}
		}
	}
}

func (d *Dispatcher) execWithRetry(ctx context.Context, h Handler, ev *event.Event, args map[string]any, ruleName, actionType string) error {
	var lastErr error
	for attempt := 0; attempt <= d.maxRetries; attempt++ {
		if attempt > 0 {
			d.Counters.Retry++
			backoff := d.backoffFor(attempt)
			slog.Debug("action: retrying", "rule", ruleName, "action", actionType, "attempt", attempt, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return fmt.Errorf("action: context cancelled during retry: %w", ctx.Err())
			}
		}
		if err := h.Execute(ctx, ev, args); err != nil {
			lastErr = err
			slog.Warn("action: attempt failed", "rule", ruleName, "action", actionType, "attempt", attempt, "error", err)
			continue
		}
		return nil
	}
	return fmt.Errorf("all %d attempts failed: %w", d.maxRetries+1, lastErr)
}

func (d *Dispatcher) backoffFor(attempt int) time.Duration {
	dur := time.Duration(float64(d.baseBackoff) * math.Pow(2, float64(attempt-1)))
	return min(dur, d.maxBackoff)
}

// --- Templating ---

// renderArgs walks args and template-renders any string that contains `{{`.
// The template context is event fields at the top level, plus `item` (current
// for-each value), plus event_id/event_source/event_type/occurred_at.
func renderArgs(args map[string]any, ev *event.Event, item any, fns *funcs.Set) (map[string]any, error) {
	ctx := make(map[string]any, len(ev.Fields)+5)
	maps.Copy(ctx, ev.Fields)
	ctx["event_id"] = ev.ID
	ctx["event_source"] = ev.Source
	ctx["event_type"] = ev.Type
	ctx["occurred_at"] = ev.OccurredAt
	ctx["item"] = item

	fm := fns.TemplateFuncMap(ctx)
	out, err := walkRender(args, ctx, fm)
	if err != nil {
		return nil, err
	}
	m, _ := out.(map[string]any)
	return m, nil
}

func walkRender(v any, ctx map[string]any, fm template.FuncMap) (any, error) {
	switch x := v.(type) {
	case string:
		if !bytes.Contains([]byte(x), []byte("{{")) {
			return x, nil
		}
		tmpl, err := template.New("").Funcs(fm).Option("missingkey=zero").Parse(x)
		if err != nil {
			return nil, fmt.Errorf("parse template %q: %w", x, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, ctx); err != nil {
			return nil, fmt.Errorf("exec template %q: %w", x, err)
		}
		return buf.String(), nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			r, err := walkRender(vv, ctx, fm)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			r, err := walkRender(vv, ctx, fm)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// fingerprint returns a stable SHA256-hex of (rule, action, iter-index, args).
func fingerprint(rule, actionType string, idx int, args map[string]any) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%d|", rule, actionType, idx)

	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	enc := json.NewEncoder(h)
	for _, k := range keys {
		fmt.Fprintf(h, "%s=", k)
		_ = enc.Encode(args[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
