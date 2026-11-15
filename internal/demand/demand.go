// Package demand reads transfer demand on an endpoint from the Globus Transfer
// endpoint_manager API (#28, #52).
package demand

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Task is the subset of an endpoint_manager task document we use.
type Task struct {
	TaskID             string `json:"task_id"`
	Status             string `json:"status"`
	EffectiveBytesPerS int64  `json:"effective_bytes_per_second"`
}

// Stats aggregates tasks.
type Stats struct {
	Count            int
	TotalBytesPerSec int64 // sum of effective_bytes_per_second
}

// Aggregate counts tasks and sums their effective rates.
func Aggregate(tasks []Task) Stats {
	s := Stats{Count: len(tasks)}
	for _, t := range tasks {
		s.TotalBytesPerSec += t.EffectiveBytesPerS
	}
	return s
}

// Lister pages through GET /v0.10/endpoint_manager/task_list.
type Lister struct {
	BaseURL string       // default https://transfer.api.globus.org/v0.10
	HTTP    *http.Client // default: 20 s timeout
	Token   string       // Transfer access token with endpoint activity-monitor rights
	Limit   int          // page size (default and max 1000)
}

type page struct {
	DATA        []Task `json:"DATA"`
	HasNextPage bool   `json:"has_next_page"`
	LastKey     string `json:"last_key"`
}

// List returns every task with the given status on endpointID, following
// last_key pagination (#28). maxPages bounds a runaway listing.
func (l Lister) List(ctx context.Context, endpointID, status string) ([]Task, error) {
	base := l.BaseURL
	if base == "" {
		base = "https://transfer.api.globus.org/v0.10"
	}
	client := l.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	limit := l.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	const maxPages = 50
	var all []Task
	lastKey := ""
	for i := 0; i < maxPages; i++ {
		q := url.Values{}
		q.Set("filter_status", status)
		q.Set("filter_endpoint", endpointID)
		q.Set("limit", fmt.Sprint(limit))
		if lastKey != "" {
			q.Set("last_key", lastKey)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/endpoint_manager/task_list?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+l.Token)
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("endpoint_manager/task_list: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read endpoint_manager/task_list: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("endpoint_manager/task_list: HTTP %d: %.300s", resp.StatusCode, body)
		}
		var p page
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("parse endpoint_manager/task_list: %w", err)
		}
		all = append(all, p.DATA...)
		if !p.HasNextPage || p.LastKey == "" {
			return all, nil
		}
		lastKey = p.LastKey
	}
	return all, fmt.Errorf("endpoint_manager/task_list: more than %d pages", maxPages)
}
