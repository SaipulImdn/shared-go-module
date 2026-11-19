package accesslog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/SaipulImdn/pixora-module/metrics"
	"github.com/SaipulImdn/pixora-module/reqid"
)

func newTestMiddleware() (*observer.ObservedLogs, func(http.Handler) http.Handler) {
	core, logs := observer.New(zap.DebugLevel)
	cfg := Config{ServiceName: "test-svc", Logger: zap.New(core)}
	return logs, Middleware(cfg)
}

func TestMiddleware_GeneratesRequestIDWhenMissing(t *testing.T) {
	logs, mw := newTestMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqid.FromContext(r.Context()) == "" {
			t.Error("expected request ID to be set in context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get(reqid.Header) == "" {
		t.Error("expected X-Request-ID response header to be set")
	}
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("got %d log entries, want 1", len(entries))
	}
	if id, ok := entries[0].ContextMap()["request_id"].(string); !ok || id == "" {
		t.Error("expected non-empty request_id field in log entry")
	}
}

func TestMiddleware_PropagatesIncomingRequestID(t *testing.T) {
	logs, mw := newTestMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	req.Header.Set(reqid.Header, "incoming-id-123")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(reqid.Header); got != "incoming-id-123" {
		t.Errorf("response X-Request-ID = %q, want incoming-id-123 (passthrough)", got)
	}
	if id := logs.All()[0].ContextMap()["request_id"]; id != "incoming-id-123" {
		t.Errorf("logged request_id = %v, want incoming-id-123", id)
	}
}

func TestMiddleware_SkipsConfiguredPaths(t *testing.T) {
	logs, mw := newTestMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if len(logs.All()) != 0 {
		t.Errorf("expected /health to be skipped, got %d log entries", len(logs.All()))
	}
}

func TestMiddleware_LogsErrorLevelOn5xx(t *testing.T) {
	logs, mw := newTestMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := logs.All()[0].Level; got != zap.ErrorLevel {
		t.Errorf("log level = %v, want error for a 5xx response", got)
	}
}

func TestMiddleware_RecordsMetricsWhenConfigured(t *testing.T) {
	core, _ := observer.New(zap.DebugLevel)
	httpMetrics := metrics.NewHTTP(metrics.Config{Prefix: "mwtest", Registerer: prometheus.NewRegistry()})
	mw := Middleware(Config{ServiceName: "test-svc", Logger: zap.New(core), Metrics: httpMetrics})

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"response_code":"00","response_desc":"ok","response_data":null}`))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/foo/123", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	m := &dto.Metric{}
	// Path must have been normalized (123 -> :id) before being used as a label.
	if err := httpMetrics.RequestsTotal.WithLabelValues("GET", "/api/v1/foo/:id", "200", "00").Write(m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	if got := m.Counter.GetValue(); got != 1 {
		t.Errorf("RequestsTotal for normalized path = %v, want 1", got)
	}
}

func TestExtractResponseEnvelope(t *testing.T) {
	body := []byte(`{"response_code":"00","response_desc":"success","response_data":{"id":1}}`)
	if got := extractStringField(body, "response_code"); got != "00" {
		t.Errorf("response_code = %q, want 00", got)
	}
	if got := extractStringField(body, "response_desc"); got != "success" {
		t.Errorf("response_desc = %q, want success", got)
	}
	if got := extractResponseField(body, "response_data"); got != `{"id":1}` {
		t.Errorf("response_data = %q, want {\"id\":1}", got)
	}
}

func logLine(t *testing.T, logs *observer.ObservedLogs) map[string]any {
	t.Helper()
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("got %d log entries, want 1", len(entries))
	}
	return entries[0].ContextMap()
}

func TestMiddleware_LogsResponseCodeDescAndFullBody(t *testing.T) {
	logs, mw := newTestMiddleware()
	payload := `{"response_code":"01","response_desc":"Invalid or expired token.","response_data":{"detail":"` + strings.Repeat("z", 12000) + `"}}`
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(payload))
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/x", nil))

	m := logLine(t, logs)
	if m["rc"] != "01" || m["response_code"] != "01" || m["response_desc"] != "Invalid or expired token." {
		t.Errorf("rc=%v response_code=%v response_desc=%v", m["rc"], m["response_code"], m["response_desc"])
	}
	// JSON is re-encoded (keys sorted), so compare content, not bytes.
	if got, _ := m["response_body"].(string); !strings.Contains(got, strings.Repeat("z", 12000)) || strings.Contains(got, "omitted") {
		t.Errorf("a 12 KB JSON response must be logged in full, got len=%d", len(got))
	}
}

func TestMiddleware_BinaryResponsesAreLoggedByTypeAndSize(t *testing.T) {
	logs, mw := newTestMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(make([]byte, 33070))
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/dedup/thumbnail", nil))

	body, _ := logLine(t, logs)["response_body"].(string)
	for _, want := range []string{`"content_type":"image/jpeg"`, `"size":33070`, `non-JSON response body`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
}

func TestMiddleware_LogsRequestJSONBodyWithCredentialsMasked(t *testing.T) {
	logs, mw := newTestMiddleware()
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/signin", strings.NewReader(`{"email":"a@b.c","password":"hunter2"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	b, _ := json.Marshal(logLine(t, logs)["request_body"])
	if strings.Contains(string(b), "hunter2") || !strings.Contains(string(b), "a@b.c") {
		t.Errorf("password must be masked and email readable: %s", b)
	}
}
