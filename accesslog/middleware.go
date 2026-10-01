// Package accesslog provides the one standard structured HTTP access-log
// middleware shared by every service. It standardizes:
//   - reading/generating the distributed-tracing request ID (see the reqid
//     package) and putting it in both the request context and the response
//     header, so callers can propagate it to their own outbound calls;
//   - a single structured log-field set across services, so a log shipper or
//     a human grepping logs doesn't need per-service parsing rules;
//   - extracting the {response_code, response_desc, response_data} envelope
//     every service wraps its responses in, without re-implementing a
//     JSON scanner in each repo.
package accesslog

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/SaipulImdn/pixora-module/metrics"
	"github.com/SaipulImdn/pixora-module/reqid"
)

// maxCaptureSize is the maximum response body bytes buffered for envelope extraction.
const maxCaptureSize = 4096

// maxResponseDataSize is the max size for response.data before truncation.
const maxResponseDataSize = 2048

// Config controls the middleware's behavior.
type Config struct {
	// ServiceName is stamped on every log line as the "service" field.
	ServiceName string
	// Logger receives one structured log line per request. Required.
	Logger *zap.Logger
	// SkipPaths are exact paths that are never logged (default: /health, /metrics).
	SkipPaths []string
	// UserID extracts a user identifier from the request context for the
	// "user_id" log field, e.g. after an auth middleware has already run and
	// populated the context. Optional — if nil, "user_id" logs as "".
	UserID func(ctx context.Context) string
	// Metrics, if set, records the standard HTTP metric set (see the metrics
	// package) for every non-skipped request — request count, duration,
	// response code, and active-connection gauge — so services don't need to
	// hand-roll this recording themselves. Optional — if nil, no metrics are
	// recorded (a service can still record its own separately).
	Metrics *metrics.HTTP
}

func (c Config) skipPaths() map[string]struct{} {
	paths := c.SkipPaths
	if paths == nil {
		paths = []string{"/health", "/metrics"}
	}
	m := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		m[p] = struct{}{}
	}
	return m
}

// Middleware returns the shared access-log HTTP middleware. It must wrap the
// full handler chain (outermost or as close to it as CORS/recovery allow) so
// the request ID it establishes is visible to every inner middleware and
// handler via reqid.FromContext.
func Middleware(cfg Config) func(http.Handler) http.Handler {
	skip := cfg.skipPaths()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := skip[r.URL.Path]; ok {
				next.ServeHTTP(w, r)
				return
			}

			id := reqid.FromHeaderOrGenerate(r.Header.Get(reqid.Header))
			w.Header().Set(reqid.Header, id)
			ctx := reqid.WithID(r.Context(), id)
			r = r.WithContext(ctx)

			clientIP := extractClientIP(r)
			reqBody, reqBodySize := CaptureRequestBody(r, 0)

			if cfg.Metrics != nil {
				cfg.Metrics.IncActive()
				defer cfg.Metrics.DecActive()
			}

			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK, body: make([]byte, 0, maxCaptureSize)}

			next.ServeHTTP(sw, r)

			duration := time.Since(start)

			responseCode := extractStringField(sw.body, "response_code")
			responseDesc := extractStringField(sw.body, "response_desc")
			responseData := extractResponseField(sw.body, "response_data")
			if len(responseData) > maxResponseDataSize {
				responseData = responseData[:maxResponseDataSize] + "...[truncated]"
			}

			var userID string
			if cfg.UserID != nil {
				userID = cfg.UserID(r.Context())
			}

			if cfg.Metrics != nil {
				cfg.Metrics.Observe(r.Method, metrics.NormalizePath(r.URL.Path), strconv.Itoa(sw.status), responseCode, duration)
			}

			fields := []zap.Field{
				zap.String("service", cfg.ServiceName),
				zap.String("request_id", id),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Float64("duration", duration.Seconds()),
				zap.String("client_ip", clientIP),
				zap.String("user_id", userID),
				zap.String("request.content_type", r.Header.Get("Content-Type")),
				zap.String("request.body", reqBody),
				zap.Int64("request.body_size", reqBodySize),
				zap.String("response.code", responseCode),
				zap.String("response.desc", responseDesc),
				zap.String("response.data", responseData),
				zap.Int("response.body_size", sw.size),
			}

			switch {
			case sw.status >= 500:
				cfg.Logger.Error("request", fields...)
			case sw.status >= 400:
				cfg.Logger.Warn("request", fields...)
			case duration >= time.Second:
				cfg.Logger.Info("request", fields...)
			default:
				cfg.Logger.Debug("request", fields...)
			}
		})
	}
}

// extractClientIP reads the first X-Forwarded-For hop, falling back to
// RemoteAddr. Unlike axe-gateway-pixora's edge-facing extractor, this doesn't
// validate a trusted-proxy allowlist: every backend service sits
// behind the gateway on an internal cluster network and is not
// internet-facing, so there's no untrusted hop between the client and here
// worth defending against at this layer.
func extractClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if ip, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(ip)
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-Ip"); xri != "" {
		return xri
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ─── Response Writer Wrapper ─────────────────────────────────────────────────

type statusWriter struct {
	http.ResponseWriter
	status      int
	size        int
	wroteHeader bool
	body        []byte
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.wroteHeader {
		sw.status = code
		sw.wroteHeader = true
		sw.ResponseWriter.WriteHeader(code)
	}
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.WriteHeader(http.StatusOK)
	}
	n, err := sw.ResponseWriter.Write(b)
	sw.size += n

	if len(sw.body) < maxCaptureSize {
		remaining := maxCaptureSize - len(sw.body)
		if len(b) > remaining {
			sw.body = append(sw.body, b[:remaining]...)
		} else {
			sw.body = append(sw.body, b...)
		}
	}

	return n, err
}

func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

// ─── Response Envelope Extraction ────────────────────────────────────────────

// extractStringField extracts a simple "key":"value" string field from JSON bytes.
func extractStringField(body []byte, field string) string {
	if len(body) == 0 {
		return ""
	}

	s := string(body)
	key := `"` + field + `":"`

	idx := strings.Index(s, key)
	if idx == -1 {
		return ""
	}

	start := idx + len(key)
	if start >= len(s) {
		return ""
	}

	end := strings.IndexByte(s[start:], '"')
	if end == -1 || end > 500 {
		return ""
	}

	return s[start : start+end]
}

// extractResponseField extracts a field's raw JSON value (object, array,
// string, or "" for null) from a JSON response body.
func extractResponseField(body []byte, field string) string {
	if len(body) == 0 {
		return ""
	}

	s := string(body)
	key := `"` + field + `":`

	idx := strings.Index(s, key)
	if idx == -1 {
		return ""
	}

	start := idx + len(key)
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	if start >= len(s) {
		return ""
	}

	if strings.HasPrefix(s[start:], "null") {
		return ""
	}

	if s[start] == '"' {
		start++
		end := strings.IndexByte(s[start:], '"')
		if end == -1 || end > maxResponseDataSize+100 {
			return ""
		}
		return s[start : start+end]
	}

	if s[start] == '{' || s[start] == '[' {
		depth := 0
		inString := false
		for i := start; i < len(s); i++ {
			ch := s[i]
			if inString {
				if ch == '"' && (i == 0 || s[i-1] != '\\') {
					inString = false
				}
				continue
			}
			switch ch {
			case '"':
				inString = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return s[start : i+1]
				}
			}
		}
	}

	return ""
}
