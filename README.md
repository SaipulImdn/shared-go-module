# pixora-module

Shared Go library for logging, distributed request tracing, and HTTP access
logs across the Ceriku platform's independent service repos (axe-gateway,
pixora-backend, clockwerk-media, rubick-profile, invoker-activity,
wisp-notifier, phantom-dedup-engine). One standard instead of seven
near-identical copies.

```
go get github.com/SaipulImdn/pixora-module
```

## Packages

### `reqid`

The distributed-tracing ID convention. axe-gateway-pixora generates or
forwards it as the `X-Request-ID` header on every proxied request; a backend
reads it (or generates one, if it's missing — e.g. a direct call that didn't
go through the gateway) and should forward it on any outbound call it makes to
another Ceriku service, so one ID can be grepped across every service's logs.

```go
id := reqid.FromHeaderOrGenerate(r.Header.Get(reqid.Header))
ctx := reqid.WithID(r.Context(), id)
// ... later, in an outbound client:
req.Header.Set(reqid.Header, reqid.FromContext(ctx))
```

### `logger`

Structured JSON zap logger construction: dual output (stdout + daily-rotated
local file), buffered file writes, consistent field encoding.

```go
log := logger.New(cfg.LogLevel, logger.Config{Dir: "logs"})
defer log.Sync()
```

### `metrics`

The standard HTTP-request Prometheus metric set: request count, latency,
response code, and active-connection gauge. Metric *names* stay per-service
(via `Config.Prefix`) since every service is scraped into the same
Prometheus/Grafana stack and existing dashboards key off names like
`gateway_http_requests_total` — only the construction code is shared, not the
metric identity.

```go
httpMetrics := metrics.NewHTTP(metrics.Config{Prefix: "pixora"}) // -> pixora_http_requests_total, etc.
```

Domain-specific metrics (e.g. `phantom_files_indexed_total`,
`rubick_password_resets_total`) stay in each service — only the generic HTTP
set belongs here.

### `accesslog`

The shared HTTP access-log middleware. Wrap it as close to the outermost
layer of your handler chain as CORS/recovery middleware allows (so the
request ID it establishes is visible to everything inside).

```go
mw := accesslog.Middleware(accesslog.Config{
    ServiceName: "pixora-backend",
    Logger:      accessLogger, // e.g. logger.NewAccess(cfg)
    UserID: func(ctx context.Context) string {
        return myauth.GetUserID(ctx) // your existing auth context helper
    },
    Metrics: httpMetrics, // optional — records the metrics package's standard
                          // set automatically, path pre-normalized via
                          // metrics.NormalizePath so IDs don't blow up label
                          // cardinality
})
handler = mw(handler)
```

Every request logs one structured line with a standard field set: `service`,
`request_id`, `method`, `path`, `duration`, `client_ip`, `user_id`,
`request.content_type`, `response.code`, `response.desc`, `response.data`,
`response.body_size` — extracted from the `{responseCode, responseDesc,
responseData}` envelope every Ceriku service already replies with. Log level
is chosen automatically: error on 5xx, warn on 4xx, info if the request took
≥1s, debug otherwise.

`accesslog`'s client-IP extraction is deliberately simple (`X-Forwarded-For` /
`X-Real-Ip` / `RemoteAddr`, no trusted-proxy allowlist) — every Ceriku backend
sits behind axe-gateway-pixora on an internal cluster network, not directly on
the internet, so there's no untrusted hop to defend against at this layer.
axe-gateway-pixora itself, being internet-facing, keeps its own stricter
trusted-proxy-validated extractor rather than using this package's.
