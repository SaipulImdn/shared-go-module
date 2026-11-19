// Package transactionlog defines the JSON contract used by Ceriku Go services.
package transactionlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Body limits. A log line must stay under Loki's 256 KiB line limit with a request
// AND a response body in it, so the per-body ceiling is 96 KiB.
const (
	minBodyLimit     = 1024
	maxBodyLimit     = 96 * 1024
	defaultBodyLimit = 64 * 1024
	previewBytes     = 2048
)

// DefaultBodyLimit is the most body bytes captured per request or response in
// one log line (default 64 KiB). Set LOG_BODY_LIMIT_BYTES to change it (clamped
// to 1 KiB..96 KiB). Bodies above it are logged as a masked preview plus the size.
var DefaultBodyLimit = bodyLimitFromEnv()

func bodyLimitFromEnv() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LOG_BODY_LIMIT_BYTES")))
	if err != nil || n <= 0 {
		return defaultBodyLimit
	}
	if n < minBodyLimit {
		return minBodyLimit
	}
	if n > maxBodyLimit {
		return maxBodyLimit
	}
	return n
}

var jakarta = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		return time.FixedZone("WIB", 7*60*60)
	}
	return loc
}()

// FormatJakartaTime encodes timestamps with the Asia/Jakarta UTC offset.
func FormatJakartaTime(value time.Time) string {
	return value.In(jakarta).Format(time.RFC3339Nano)
}

// RequestHeader contains only client and device metadata, never credentials.
type RequestHeader struct {
	DeviceID    string `json:"device-id"`
	DeviceType  int    `json:"device-type"`
	DeviceModel string `json:"device-model"`
	AppVersion  string `json:"app-version"`
	OSVersion   string `json:"os-version"`
	IPAddress   string `json:"ip-address"`
}

// Event is the common transaction-log shape. Body fields are bounded JSON
// values after sanitization; response_body is a JSON string to match the
// existing operations log contract.
type Event struct {
	Timestamp        string        `json:"timestamp"`
	Level            string        `json:"level"`
	Message          string        `json:"msg"`
	Event            string        `json:"event"`
	Transaction      string        `json:"trx_type"`
	Feature          string        `json:"trx_feature"`
	Service          string        `json:"service"`
	Endpoint         string        `json:"endpoint"`
	TimeIn           string        `json:"time_in"`
	TimeOut          string        `json:"time_out"`
	TimeResponse     int64         `json:"time_response"`
	ResponseTime     int64         `json:"response_time"`
	RequestID        string        `json:"request_id"`
	RequestIDLegacy  string        `json:"requestId"`
	SessionID        string        `json:"session_id"`
	SessionIDLegacy  string        `json:"sessionId,omitempty"`
	Username         string        `json:"username"`
	UserID           string        `json:"user_id"`
	RC               string        `json:"rc"`
	ResponseCode     string        `json:"response_code"`
	ResponseDesc     string        `json:"response_desc"`
	HTTPCode         int           `json:"http_code"`
	RequestHeader    RequestHeader `json:"request_header"`
	RequestBody      any           `json:"request_body"`
	ResponseBody     string        `json:"response_body"`
	Debug            string        `json:"debug"`
	RequestBodySize  int64         `json:"request_body_size"`
	ResponseBodySize int64         `json:"response_body_size"`
	UpstreamService  string        `json:"upstream_service,omitempty"`
	UpstreamStatus   string        `json:"upstream_http_code,omitempty"`
	UpstreamTimeMS   int64         `json:"upstream_response_time_ms"`

	// Compatibility aliases keep existing service dashboards usable during rollout.
	Method   string `json:"method"`
	Path     string `json:"path"`
	Route    string `json:"route"`
	Status   int    `json:"status"`
	Duration int64  `json:"durationMs"`
}

// NewEvent creates a request event with consistent Jakarta timestamps and
// endpoint-derived feature labels.
func NewEvent(service, method, path, requestID, sessionID, username, userID string, status int, rc string, started, finished time.Time, headers RequestHeader, requestBody, responseBody []byte, requestBodySize, responseBodySize int64, omitBody bool) Event {
	if finished.IsZero() {
		finished = time.Now()
	}
	if started.IsZero() {
		started = finished
	}
	if rc == "" {
		rc = fmt.Sprint(status)
	}
	requestValue := any(map[string]any{"omitted": "body logging disabled"})
	responseValue := `{"omitted":"body logging disabled"}`
	if !omitBody {
		if len(requestBody) > 0 {
			requestValue = decode(sanitize(requestBody, DefaultBodyLimit))
		} else {
			requestValue = map[string]any{"omitted": "body not captured", "size": max64(requestBodySize, 0)}
		}
		if len(responseBody) > 0 {
			responseValue = string(sanitize(responseBody, DefaultBodyLimit))
		} else {
			responseValue = `{"omitted":"body not captured"}`
		}
	}
	ms := finished.Sub(started).Milliseconds()
	if ms < 0 {
		ms = 0
	}
	level := "info"
	if status >= 500 {
		level = "error"
	} else if status >= 400 {
		level = "warn"
	}
	return Event{
		Timestamp: FormatJakartaTime(finished), Level: level,
		Message: "request completed", Event: "http.request.completed",
		Transaction: TransactionType(method, path), Feature: FeatureFromPath(path),
		Service: service, Endpoint: NormalizeEndpoint(path),
		TimeIn: FormatJakartaTime(started), TimeOut: FormatJakartaTime(finished),
		TimeResponse: ms, ResponseTime: ms, RequestID: requestID, RequestIDLegacy: requestID,
		SessionID: sessionID, SessionIDLegacy: sessionID,
		Username: username, UserID: userID, RC: rc, ResponseCode: rc, ResponseDesc: responseDesc(responseValue),
		HTTPCode: status, RequestHeader: headers,
		RequestBody: requestValue, ResponseBody: responseValue,
		RequestBodySize: requestBodySize, ResponseBodySize: responseBodySize,
		Method: method, Path: NormalizeEndpoint(path), Route: NormalizeEndpoint(path), Status: status, Duration: ms,
	}
}

// TransactionType uses a trusted server-side label when present, otherwise
// the HTTP method. Callers should validate any supplied label before passing it.
func TransactionType(method, path string) string {
	return strings.ToUpper(strings.TrimSpace(method))
}

// FeatureFromPath groups endpoints by the first path segment after /api/vN.
func FeatureFromPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "api" && strings.HasPrefix(parts[1], "v") {
		return parts[2]
	}
	if len(parts) > 0 && parts[0] != "" {
		return parts[0]
	}
	return "other"
}

// NormalizeEndpoint removes per-user and per-file path values before logging.
func NormalizeEndpoint(path string) string {
	path = strings.SplitN(path, "?", 2)[0]
	const filesPrefix = "/api/v1/storage/files/"
	if strings.HasPrefix(path, filesPrefix) {
		rest := strings.Trim(strings.TrimPrefix(path, filesPrefix), "/")
		parts := strings.Split(rest, "/")
		if len(parts) > 1 {
			switch parts[len(parts)-1] {
			case "download", "thumbnail", "editor", "image-insert-token":
				return filesPrefix + ":file/" + parts[len(parts)-1]
			}
		}
		return filesPrefix + ":file"
	}
	parts := strings.Split(path, "/")
	for index, part := range parts {
		if isUUID(part) || isHexID(part) || isNumeric(part) {
			parts[index] = ":id"
		}
	}
	return strings.Join(parts, "/")
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if char != '-' {
				return false
			}
		} else if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func isNumeric(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func isHexID(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

// SanitizeJSON returns a bounded, JSON-safe value and masks credentials and
// account-like identifiers. Non-JSON or oversized bodies are represented by
// a reason and byte count; raw fallback text is never logged.
func SanitizeJSON(body []byte, limit int) any {
	return decode(sanitize(body, limit))
}

// SanitizeJSONText returns sanitized JSON as a string for response_body.
func SanitizeJSONText(body []byte, limit int) string {
	return string(sanitize(body, limit))
}

// OmittedBody builds a structured placeholder for body data that must not be captured.
func OmittedBody(reason string, size int64) []byte {
	value, _ := json.Marshal(map[string]any{"omitted": reason, "size": max64(size, 0)})
	return value
}

func sanitize(body []byte, limit int) []byte {
	if limit <= 0 {
		limit = DefaultBodyLimit
	}
	if len(body) == 0 {
		return []byte(`{}`)
	}
	if len(body) > limit {
		return TruncatedBody(body[:limit], int64(len(body)))
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		b, _ := json.Marshal(map[string]any{"omitted": "non-json body", "size": len(body)})
		return b
	}
	sanitizeValue(value)
	b, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"omitted":"body could not be encoded"}`)
	}
	return b
}

func decode(body []byte) any {
	var value any
	if json.Unmarshal(body, &value) != nil {
		return map[string]any{"omitted": "body could not be decoded"}
	}
	return value
}

func sanitizeValue(value any) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
			if isCredential(normalized) {
				current[key] = "***"
				continue
			}
			if isFinancialIdentifier(normalized) {
				current[key] = maskIdentifier(child)
				continue
			}
			if value, ok := child.(string); ok && isSecretValue(value) {
				current[key] = "***"
				continue
			}
			sanitizeValue(child)
		}
	case []any:
		for _, child := range current {
			sanitizeValue(child)
		}
	}
}

func isCredential(key string) bool {
	for _, part := range []string{"password", "passwd", "token", "secret", "pin", "apikey", "authorization", "cookie", "signature", "credential", "privatekey", "presigned", "signedurl", "proofdataurl", "proofimage", "proofurl", "buktitransfer"} {
		if key == part || strings.HasSuffix(key, part) {
			return true
		}
	}
	return strings.HasPrefix(key, "otp")
}

func isSecretValue(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "x-amz-signature=") || strings.Contains(lower, "access_token=") ||
		strings.Contains(lower, "x-amz-credential=") || strings.Contains(lower, "x-amz-security-token=") ||
		strings.Contains(lower, "refresh_token=") || strings.Contains(lower, "authorization=bearer") ||
		strings.Contains(lower, "?token=") || strings.Contains(lower, "&token=") ||
		strings.Contains(lower, "?signature=") || strings.Contains(lower, "&signature=")
}

func isFinancialIdentifier(key string) bool {
	if strings.Contains(key, "amount") || strings.Contains(key, "balance") || strings.Contains(key, "accounttype") || strings.Contains(key, "accountname") {
		return false
	}
	if key == "bankaccount" {
		return false
	}
	for _, part := range []string{"destinationaccount", "targetaccount", "beneficiaryaccount", "recipientaccount", "rekeningtujuan", "paymentnumber", "paymentno", "nomorpembayaran", "virtualaccount"} {
		if strings.Contains(key, part) {
			return false
		}
	}
	for _, part := range []string{"account", "rekening", "cardnumber", "msisdn", "phonenumber", "mobilephone"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}

func maskIdentifier(value any) any {
	switch current := value.(type) {
	case string:
		if len(current) <= 4 {
			return "***"
		}
		return "***" + current[len(current)-4:]
	case json.Number:
		return "***" + lastFour(string(current))
	case float64:
		return "***" + lastFour(fmt.Sprintf("%.0f", current))
	default:
		return "***"
	}
}

func lastFour(value string) string {
	if len(value) <= 4 {
		return value
	}
	return value[len(value)-4:]
}

// TruncatedBody describes a body that exceeded the log limit: its size, the
// limit, and a short preview of the start with credentials masked. prefix is
// whatever was captured of the body (at most the limit); total is the real size.
func TruncatedBody(prefix []byte, total int64) []byte {
	n := len(prefix)
	if n > previewBytes {
		n = previewBytes
	}
	preview := strings.ToValidUTF8(string(prefix[:n]), "")
	value, _ := json.Marshal(map[string]any{
		"omitted": "body exceeds log limit", "size": max64(total, 0), "limit": DefaultBodyLimit,
		"preview": maskRaw(preview),
	})
	return value
}

// OmittedBodyType is OmittedBody plus the content type, for bodies that are not
// logged because they are binary (images, video, archives, uploads).
func OmittedBodyType(reason string, size int64, contentType string) []byte {
	m := map[string]any{"omitted": reason, "size": max64(size, 0)}
	if contentType = strings.TrimSpace(contentType); contentType != "" {
		m["content_type"] = contentType
	}
	value, _ := json.Marshal(m)
	return value
}

var (
	rawCredentialJSON = regexp.MustCompile(`(?i)("[a-z0-9_\-]*(?:password|passwd|token|secret|pin|apikey|authorization|cookie|signature|credential|privatekey|presigned|signedurl|otp)[a-z0-9_\-]*"\s*:\s*)"[^"]*"`)
	rawCredentialURL  = regexp.MustCompile(`(?i)(x-amz-signature|x-amz-credential|x-amz-security-token|access_token|refresh_token|signature|token)=[^&"\s]+`)
)

// maskRaw masks credentials in text that could not be parsed as JSON (a
// truncated preview), where the key-based JSON sanitizer cannot run.
func maskRaw(text string) string {
	text = rawCredentialJSON.ReplaceAllString(text, `$1"***"`)
	return rawCredentialURL.ReplaceAllString(text, `$1=***`)
}

// responseDesc reads the human-readable description from a JSON response body
// ({"response_desc": ...} / {"responseDesc": ...} / {"message": ...}).
func responseDesc(body string) string {
	if !strings.HasPrefix(strings.TrimSpace(body), "{") {
		return ""
	}
	var env map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &env) != nil {
		return ""
	}
	for _, key := range []string{"response_desc", "responseDesc", "message"} {
		var v string
		if raw, ok := env[key]; ok && json.Unmarshal(raw, &v) == nil && v != "" {
			if !utf8.ValidString(v) {
				v = strings.ToValidUTF8(v, "")
			}
			return v
		}
	}
	return ""
}
