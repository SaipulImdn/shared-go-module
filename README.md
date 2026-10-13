# pixora-module

Shared Go library for logging, distributed request tracing, and HTTP access
logs across Ceriku's independent service repos. One standard instead of
service-specific logging formats.

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

### `transactionlog`

The common structured event schema and outbound HTTP transport. Events use
Jakarta timestamps and include `trx_type`, `trx_feature`, `service`,
`time_in`, `time_out`, `time_response`, `request_id`, `session_id`,
`username`, `user_id`, `rc`, `http_code`, `request_header`, `request_body`,
and `response_body`. JSON capture is capped at 4 KiB; credentials are removed
and account/payment identifiers are masked. Binary, unknown-length, and
oversized bodies are represented by an omission marker and byte count.

Wrap custom `http.RoundTripper`s with `transactionlog.NewTransport`, or call
`InstallDefaultTransport` once at startup. Each downstream call emits a
separate `http.client.completed` event with the same request ID, its endpoint,
HTTP result, and response time. This keeps fan-out and retries searchable in
Loki without fixed `s_1`/`s_2` fields.

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

Every request logs the `transactionlog.Event` schema. Authenticated `user_id`
and `username` are read only from callbacks backed by the service's verified
auth context; fields remain empty when that identity is unavailable. HTTP
health and metrics paths are skipped by each service's config.

`accesslog`'s client-IP extraction is deliberately simple (`X-Forwarded-For` /
`X-Real-Ip` / `RemoteAddr`, no trusted-proxy allowlist) — every Ceriku backend
sits behind axe-gateway-pixora on an internal cluster network, not directly on
the internet, so there's no untrusted hop to defend against at this layer.
axe-gateway-pixora itself, being internet-facing, keeps its own stricter
trusted-proxy-validated extractor rather than using this package's.
