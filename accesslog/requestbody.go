package accesslog

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/SaipulImdn/pixora-module/transactionlog"
)

// DefaultMaxRequestBody is the largest request body CaptureRequestBody will
// log in full; anything at or above this size is reported by size only.
const DefaultMaxRequestBody = 4096

// CaptureRequestBody reads a request's JSON body for access-log purposes and
// restores r.Body so the real handler still sees it unchanged. It never
// buffers what it shouldn't:
//   - non-JSON bodies, including file uploads, are reported by size only;
//   - a body of unknown length (chunked, no Content-Length) is left
//     completely untouched — never read, never logged;
//   - a body at or above maxBody bytes (0 = DefaultMaxRequestBody) is
//     reported by size only, not read.
//
// Known-sensitive fields (password, token, secret, otp, pin, apiKey,
// accessToken, refreshToken) are redacted before the body is returned.
//
// Call this before next.ServeHTTP so the restored body reaches the handler.
func CaptureRequestBody(r *http.Request, maxBody int) (body string, size int64) {
	if maxBody <= 0 {
		maxBody = DefaultMaxRequestBody
	}
	if r.Body == nil {
		return "", 0
	}
	if r.ContentLength <= 0 {
		return "", 0
	}

	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "json") {
		if strings.Contains(contentType, "multipart/form-data") || strings.Contains(contentType, "octet-stream") {
			return "[binary/multipart]", r.ContentLength
		}
		return "[omitted: non-JSON body]", r.ContentLength
	}
	if r.ContentLength >= int64(maxBody) {
		return fmt.Sprintf("[large body: %d bytes]", r.ContentLength), r.ContentLength
	}

	bodyBytes, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	if err != nil {
		return "", int64(len(bodyBytes))
	}
	if len(bodyBytes) == 0 {
		return "", 0
	}
	return transactionlog.SanitizeJSONText(bodyBytes, maxBody), int64(len(bodyBytes))
}
