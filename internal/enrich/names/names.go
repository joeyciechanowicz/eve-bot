// Package names enriches killmail events with character, corporation, and
// alliance names resolved via the EVE ESI bulk-names endpoint. Resolved names
// are cached in a NameCache (backed by *store.Store in production) with a
// configurable TTL so repeated kills involving the same actors don't re-hit
// ESI.
package names

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/joeyciechanowicz/eve-bot/event"
)

const (
	cacheScope     = "esi_name"
	defaultBaseURL = "https://esi.evetech.net"
	defaultTTL     = 7 * 24 * time.Hour
	userAgent      = "zkill-bot/2.0 (names)"
)

// NameCache is the subset of *store.Store needed for caching ESI name lookups.
type NameCache interface {
	Get(scope, key string) ([]byte, bool)
	Put(scope, key string, value any, ttl time.Duration) error
}

// Enricher resolves character/corporation/alliance IDs on a killmail event
// into human-readable names.
type Enricher struct {
	client  *http.Client
	cache   NameCache
	ttl     time.Duration
	baseURL string
}

// New builds an Enricher. A nil cache disables caching. ttl<=0 falls back to
// the default (7 days). baseURL="" falls back to the production ESI host.
func New(client *http.Client, cache NameCache, ttl time.Duration, baseURL string) *Enricher {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Enricher{client: client, cache: cache, ttl: ttl, baseURL: baseURL}
}

// cachedName is the JSON shape persisted per-ID.
type cachedName struct {
	Name     string `json:"name"`
	Category string `json:"category"`
}

// Enrich annotates the event with character_name / corporation_name /
// alliance_name fields on victim, every attacker, and final_blow. ESI failures
// are logged and swallowed so the event still flows through the pipeline.
func (e *Enricher) Enrich(ctx context.Context, ev *event.Event) error {
	holders := collectActors(ev)
	if len(holders) == 0 {
		return nil
	}

	ids := make([]int64, 0, len(holders))
	seen := make(map[int64]bool, len(holders))
	for _, h := range holders {
		if seen[h.id] {
			continue
		}
		seen[h.id] = true
		ids = append(ids, h.id)
	}

	resolved := make(map[int64]string, len(ids))
	var misses []int64
	if e.cache != nil {
		for _, id := range ids {
			if raw, ok := e.cache.Get(cacheScope, strconv.FormatInt(id, 10)); ok {
				var cn cachedName
				if err := json.Unmarshal(raw, &cn); err == nil && cn.Name != "" {
					resolved[id] = cn.Name
					continue
				}
			}
			misses = append(misses, id)
		}
	} else {
		misses = ids
	}

	if len(misses) > 0 {
		fetched, err := e.lookup(ctx, misses)
		if err != nil {
			slog.Warn("names: esi lookup failed", "event_id", ev.ID, "unresolved", len(misses), "error", err)
		} else {
			for _, r := range fetched {
				resolved[r.ID] = r.Name
				if e.cache != nil {
					_ = e.cache.Put(cacheScope, strconv.FormatInt(r.ID, 10), cachedName{Name: r.Name, Category: r.Category}, e.ttl)
				}
			}
		}
	}

	for _, h := range holders {
		if name, ok := resolved[h.id]; ok {
			h.holder[h.field] = name
		}
	}
	return nil
}

// actorRef points at a destination slot for a resolved name.
type actorRef struct {
	holder map[string]any // the map to write the name field into
	field  string         // e.g. "character_name"
	id     int64
}

// collectActors walks the event and returns every (holder, field, id) slot
// that needs a name. Zero IDs (NPCs / no-alliance) are skipped.
func collectActors(ev *event.Event) []actorRef {
	var out []actorRef
	if v, ok := ev.Fields["victim"].(map[string]any); ok {
		out = appendActorRefs(out, v)
	}
	if attackers, ok := ev.Fields["attackers"].([]any); ok {
		for _, a := range attackers {
			if am, ok := a.(map[string]any); ok {
				out = appendActorRefs(out, am)
			}
		}
	}
	// final_blow shares its map with the matching attackers[i] entry (set up
	// in normalize.go), so it's already covered by the attackers loop above.
	// Nothing extra to do here.
	return out
}

func appendActorRefs(out []actorRef, m map[string]any) []actorRef {
	for _, pair := range [...]struct {
		idKey, nameKey string
	}{
		{"character_id", "character_name"},
		{"corporation_id", "corporation_name"},
		{"alliance_id", "alliance_name"},
	} {
		id, ok := m[pair.idKey].(int64)
		if !ok || id == 0 {
			continue
		}
		out = append(out, actorRef{holder: m, field: pair.nameKey, id: id})
	}
	return out
}
