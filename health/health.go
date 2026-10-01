// Package health provides the standard liveness and readiness HTTP handlers for
// services.
//
//   - Liveness ("/health"): a static 200. Must never depend on a downstream, or
//     a transient DB/Redis blip would make Kubernetes kill an otherwise-fine pod.
//   - Readiness ("/readyz"): pings each registered dependency; 200 only when all
//     pass, 503 otherwise, so traffic is withheld until the pod can actually serve.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Check is one named readiness dependency (e.g. "mysql", "redis").
type Check struct {
	Name string
	Ping func(ctx context.Context) error
}

// Liveness returns a handler that always answers 200 with build info.
func Liveness(info map[string]string) http.HandlerFunc {
	body := map[string]any{"status": "ok"}
	for k, v := range info {
		body[k] = v
	}
	payload, _ := json.Marshal(body)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(payload)
	}
}

// Readiness returns a handler that pings every check (each bounded by timeout,
// default 2s) and answers 200 {"status":"ready"} or 503 with the per-check result.
func Readiness(timeout time.Duration, checks ...Check) http.HandlerFunc {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		results := make(map[string]string, len(checks))
		ready := true
		for _, c := range checks {
			if err := c.Ping(ctx); err != nil {
				results[c.Name] = "down"
				ready = false
			} else {
				results[c.Name] = "ok"
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		status := "ready"
		if !ready {
			status = "not ready"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "checks": results})
	}
}
