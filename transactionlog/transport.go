package transactionlog

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SaipulImdn/pixora-module/reqid"
	"go.uber.org/zap"
)

type loggedTransport struct {
	service string
	logger  *zap.Logger
	base    http.RoundTripper
}

// InstallDefaultTransport records outgoing HTTP service hops and propagates
// request IDs for clients that use the standard transport. Call during startup.
func InstallDefaultTransport(service string, logger *zap.Logger) {
	base := http.DefaultTransport
	if base == nil {
		return
	}
	if _, installed := base.(*loggedTransport); installed {
		return
	}
	http.DefaultTransport = NewTransport(service, logger, base)
}

// NewTransport wraps a custom transport with the same service-hop logging.
func NewTransport(service string, logger *zap.Logger, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &loggedTransport{service: service, logger: logger, base: base}
}

func (t *loggedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	id := reqid.FromContext(request.Context())
	if id == "" {
		id = reqid.FromHeaderOrGenerate(request.Header.Get(reqid.Header))
	}
	request.Header.Set(reqid.Header, id)
	started := time.Now()
	body, bodySize := outgoingBody(request)
	response, err := t.base.RoundTrip(request)
	if err != nil {
		finished := time.Now()
		entry := NewEvent(t.service, request.Method, requestPath(request), id, request.Header.Get("X-Session-ID"), "", "", 0, "transport_error", started, finished, requestHeader(request), body, OmittedBody("upstream request failed", 0), bodySize, 0, false)
		entry.Event = "http.client.completed"
		entry.Level = "error"
		entry.UpstreamService = request.URL.Host
		entry.UpstreamTimeMS = finished.Sub(started).Milliseconds()
		entry.Endpoint = endpoint(request)
		entry.Path = entry.Endpoint
		Log(t.logger, entry)
		return nil, err
	}
	response.Body = &loggedResponseBody{
		ReadCloser: response.Body, logger: t.logger, service: t.service, request: request,
		requestID: id, requestBody: body, requestSize: bodySize, started: started,
		status: response.StatusCode, contentType: response.Header.Get("Content-Type"), upstream: request.URL.Host,
	}
	return response, nil
}

func (t *loggedTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

type loggedResponseBody struct {
	io.ReadCloser
	logger       *zap.Logger
	service      string
	request      *http.Request
	requestID    string
	requestBody  []byte
	requestSize  int64
	started      time.Time
	status       int
	contentType  string
	upstream     string
	response     bytes.Buffer
	responseSize int64
	once         sync.Once
}

func (body *loggedResponseBody) Read(buffer []byte) (int, error) {
	n, err := body.ReadCloser.Read(buffer)
	body.responseSize += int64(n)
	if n > 0 && isJSON(body.contentType) && body.response.Len() < DefaultBodyLimit {
		remaining := DefaultBodyLimit - body.response.Len()
		captured := n
		if captured > remaining {
			captured = remaining
		}
		_, _ = body.response.Write(buffer[:captured])
	}
	if err == io.EOF {
		body.finish()
	}
	return n, err
}

func (body *loggedResponseBody) Close() error {
	err := body.ReadCloser.Close()
	body.finish()
	return err
}

func (body *loggedResponseBody) finish() {
	body.once.Do(func() {
		finished := time.Now()
		responseBody := body.response.Bytes()
		if !isJSON(body.contentType) && body.responseSize > 0 {
			responseBody = OmittedBody("non-JSON response body", body.responseSize)
		} else if body.responseSize > DefaultBodyLimit {
			responseBody = OmittedBody("body exceeds log limit", body.responseSize)
		}
		rc := strconv.Itoa(body.status)
		var envelope struct {
			ResponseCode string `json:"response_code"`
		}
		if json.Unmarshal(responseBody, &envelope) == nil && envelope.ResponseCode != "" {
			rc = envelope.ResponseCode
		}
		entry := NewEvent(body.service, body.request.Method, requestPath(body.request), body.requestID,
			body.request.Header.Get("X-Session-ID"), "", "", body.status, rc, body.started, finished,
			requestHeader(body.request), body.requestBody, responseBody, body.requestSize, body.responseSize, false)
		entry.Event = "http.client.completed"
		entry.UpstreamService = body.upstream
		entry.UpstreamStatus = strconv.Itoa(body.status)
		entry.UpstreamTimeMS = finished.Sub(body.started).Milliseconds()
		entry.Endpoint = endpoint(body.request)
		entry.Path = entry.Endpoint
		Log(body.logger, entry)
	})
}

func outgoingBody(request *http.Request) ([]byte, int64) {
	if request.Body == nil || !isJSON(request.Header.Get("Content-Type")) || request.ContentLength <= 0 {
		return nil, max64(0, request.ContentLength)
	}
	if request.ContentLength > DefaultBodyLimit {
		return OmittedBody("body exceeds log limit", request.ContentLength), request.ContentLength
	}
	raw, err := io.ReadAll(request.Body)
	request.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return nil, int64(len(raw))
	}
	return raw, int64(len(raw))
}

func requestHeader(request *http.Request) RequestHeader {
	deviceType, _ := strconv.Atoi(request.Header.Get("X-Device-Type"))
	if deviceType != 1 && deviceType != 2 {
		deviceType = 0
	}
	return RequestHeader{
		DeviceID: request.Header.Get("X-Device-ID"), DeviceType: deviceType,
		DeviceModel: request.Header.Get("X-Device-Model"), AppVersion: request.Header.Get("X-App-Version"),
		OSVersion: request.Header.Get("X-OS-Version"), IPAddress: firstIP(request.Header.Get("X-Forwarded-For")),
	}
}

func endpoint(request *http.Request) string {
	if request.URL == nil {
		return "unknown"
	}
	return request.URL.Host + NormalizeEndpoint(request.URL.EscapedPath())
}

func requestPath(request *http.Request) string {
	if request.URL == nil {
		return "unknown"
	}
	return NormalizeEndpoint(request.URL.EscapedPath())
}

func isJSON(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "json")
}

func firstIP(value string) string {
	if first, _, found := strings.Cut(value, ","); found {
		return strings.TrimSpace(first)
	}
	return strings.TrimSpace(value)
}

func max64(value, minimum int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
}
