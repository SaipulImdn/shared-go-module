// Package recovery provides the one standard panic-recovery middleware shared by
// every HTTP service. Applied as the outermost layer, it turns a panic in
// any handler or inner middleware into:
//   - a single structured error log (with the reqid request ID + a full stack), and
//   - a clean 500 response in the {response_code, response_desc, response_data}
//     envelope every service uses,
//
// instead of Go's default (a dropped connection and an unstructured stdlib stack
// trace). http.ErrAbortHandler is re-panicked, never swallowed — it's the
// documented way to abort a request silently.
package recovery

import (
	"net/http"
	"runtime/debug"

	"go.uber.org/zap"

	"github.com/SaipulImdn/pixora-module/reqid"
)

// body500 is the canonical HTTP 500 envelope. "ISE" = Internal Server Error.
const body500 = `{"response_code":"ISE","response_desc":"Internal server error","response_data":null}`

// Middleware returns the panic-recovery middleware. logger is required.
func Middleware(logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &writer{ResponseWriter: w}
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				logger.Error("panic recovered",
					zap.Any("panic", rec),
					zap.String("request_id", reqid.FromContext(r.Context())),
					zap.String("method", r.Method),
					zap.String("path", r.URL.Path),
					zap.ByteString("stack", debug.Stack()),
				)
				if !sw.wroteHeader {
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(body500))
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// writer tracks whether the response has started, so recovery only writes its
// own 500 when nothing has been sent yet.
type writer struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *writer) WriteHeader(code int) {
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *writer) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }
