package names

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// esiBatchSize is the documented max IDs per /universe/names/ request.
const esiBatchSize = 1000

// esiName is one entry in the ESI bulk-names response.
type esiName struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

// lookup posts ids to /v3/universe/names/, chunking when over esiBatchSize.
// Returns whatever was successfully resolved; per-chunk failures abort.
func (e *Enricher) lookup(ctx context.Context, ids []int64) ([]esiName, error) {
	var out []esiName
	for start := 0; start < len(ids); start += esiBatchSize {
		end := min(start+esiBatchSize, len(ids))
		batch, err := e.postNames(ctx, ids[start:end])
		if err != nil {
			return out, err
		}
		out = append(out, batch...)
	}
	return out, nil
}

func (e *Enricher) postNames(ctx context.Context, ids []int64) ([]esiName, error) {
	body, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("marshal ids: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/v3/universe/names/", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("esi names: status %d: %s", resp.StatusCode, string(snippet))
	}

	var names []esiName
	if err := json.NewDecoder(resp.Body).Decode(&names); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return names, nil
}
