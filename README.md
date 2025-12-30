# cio

A high-performance, feature-rich HTTP client library for Go with streaming support, retry, rate limiting, circuit breaker, and more.

## Installation

```bash
go get github.com/dreamph/cio
```

## Features

- **Simple API** - Clean, intuitive interface
- **Streaming** - Upload/download large files without buffering
- **Multipart Upload** - Easy file uploads with form fields
- **Retry** - Configurable retry with custom conditions and jitter
- **Rate Limiting** - Token bucket rate limiter with context support
- **Circuit Breaker** - Automatic circuit breaker pattern
- **Interceptors** - Request/response/metrics hooks
- **Cookie Jar** - Automatic cookie management
- **Redirect Control** - Limit or disable redirects
- **Request Tracing** - Built-in request ID and distributed tracing support
- **Timeout** - Per-request and client-level timeout
- **Status Validation** - Auto error on unexpected status codes
- **Compression** - Gzip request/response support
- **Debug & Dump** - Request/response debugging and dumping
- **Caching** - HTTP cache control headers
- **Range Requests** - Partial content support
- **Conditional Requests** - ETag and Last-Modified support
- **Testing** - Built-in mock transport and recording

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "github.com/dreamph/cio"
)

func main() {
    c := cio.New(cio.BaseURL("https://api.example.com"))
    ctx := context.Background()

    // GET request
    resp, err := c.Get(ctx, "/users")
    if err != nil {
        panic(err)
    }
    fmt.Println(resp.String())

    // POST with JSON
    resp, err = c.Post(ctx, "/users",
        cio.Body(map[string]string{"name": "John"}),
        cio.Headers(cio.ContentType(cio.JSON)),
    )
}
```

## Usage

### Basic Requests

```go
c := cio.New(cio.BaseURL("https://api.example.com"))
ctx := context.Background()

// GET
resp, _ := c.Get(ctx, "/users")

// POST
resp, _ := c.Post(ctx, "/users", cio.Body(user))

// PUT
resp, _ := c.Put(ctx, "/users/1", cio.Body(user))

// PATCH
resp, _ := c.Patch(ctx, "/users/1", cio.Body(updates))

// DELETE
resp, _ := c.Delete(ctx, "/users/1")
```

### Response Handling

```go
resp, _ := c.Get(ctx, "/users")

// As bytes
data := resp.Body

// As string
str := resp.String()

// As JSON (with generics)
users, err := cio.Json[[]User](resp)

// As JSON (traditional)
var users []User
err := resp.Json(&users)

// Check status
if resp.OK() { // 2xx
    // success
}
```

### Query Parameters

```go
// Individual params
c.Get(ctx, "/users", cio.Query("page", "1"), cio.Query("limit", "10"))

// Map
c.Get(ctx, "/users", cio.QueryMap(map[string]string{
    "page":  "1",
    "limit": "10",
}))

// Using R struct
c.Get(ctx, "/users", cio.R{
    Query: map[string]string{"page": "1", "limit": "10"},
})
```

### Headers

```go
// Content-Type
c.Post(ctx, "/users", cio.Headers(cio.ContentType(cio.JSON)))

// Bearer token
c.Get(ctx, "/profile", cio.Headers(cio.Bearer("my-token")))

// Custom headers
c.Get(ctx, "/api", cio.Headers(
    cio.Header("X-Custom", "value"),
    cio.Accept(cio.JSON),
))

// Multiple headers using R struct
c.Post(ctx, "/api", cio.R{
    Headers: map[string]string{
        "Content-Type": cio.JSON,
        "X-Api-Key":    "secret",
    },
    Body: data,
})
```

### Streaming

```go
// Download to file (no buffering)
file, _ := os.Create("large-file.zip")
resp, _ := c.Get(ctx, "/files/large.zip", cio.OutputStream(file))
file.Close()
fmt.Printf("Downloaded %d bytes\n", resp.Written)

// Upload from file (streaming)
file, _ := os.Open("upload.zip")
resp, _ := c.Post(ctx, "/upload",
    cio.BodyReader(file),
    cio.Headers(cio.ContentType(cio.OctetStream)),
)
file.Close()

// Stream to stdout
c.Get(ctx, "/api/stream", cio.OutputStream(os.Stdout))
```

### Multipart Upload

```go
// Single file
file, _ := os.Open("photo.jpg")
resp, _ := c.Post(ctx, "/upload",
    cio.Files(cio.NewFile("file", "photo.jpg", file)),
)
file.Close()

// Multiple files
f1, _ := os.Open("photo1.jpg")
f2, _ := os.Open("photo2.jpg")
resp, _ := c.Post(ctx, "/upload",
    cio.Files(
        cio.NewFile("files", "photo1.jpg", f1),
        cio.NewFile("files", "photo2.jpg", f2),
    ),
)

// Files with form fields
file, _ := os.Open("document.pdf")
resp, _ := c.Post(ctx, "/upload",
    cio.Files(cio.NewFile("document", "document.pdf", file)),
    cio.FormFields(map[string]string{
        "title":       "My Document",
        "description": "Important file",
    }),
)

// Using Multipart struct
resp, _ := c.Post(ctx, "/upload", cio.Multipart{
    Files: []cio.File{
        cio.NewFile("avatar", "avatar.png", file),
    },
    Fields: map[string]string{
        "username": "john",
    },
})

// Upload from io.Reader (not just files)
data := strings.NewReader("hello world")
resp, _ := c.Post(ctx, "/upload",
    cio.Files(cio.NewFile("file", "hello.txt", data)),
)
```

### Timeout

```go
// Per-request timeout (milliseconds)
resp, err := c.Get(ctx, "/slow-api", cio.Timeout(5000))

// Using R struct
resp, err := c.Get(ctx, "/api", cio.R{
    Timeout: 5 * time.Second,
})
```

### Retry

```go
// Basic retry (3 attempts, 100ms base, 5000ms max backoff)
resp, err := c.Get(ctx, "/api", cio.Retry(3, 100, 5000))

// Retry on 5xx only
resp, err := c.Get(ctx, "/api", cio.Retry(3, 100, 5000, cio.When5xx()))

// Retry on specific status codes
resp, err := c.Get(ctx, "/api", cio.Retry(3, 100, 5000, cio.WhenStatus(500, 502, 503, 429)))

// Retry on specific errors
resp, err := c.Get(ctx, "/api", cio.Retry(3, 100, 5000, cio.WhenErr(func(err error) bool {
    return errors.Is(err, context.DeadlineExceeded)
})))

// Full custom condition
resp, err := c.Get(ctx, "/api", cio.Retry(3, 100, 5000, cio.When(func(resp *cio.Response, err error) bool {
    if err != nil {
        return true // retry on any error
    }
    return resp.StatusCode == 429 || resp.StatusCode >= 500
})))

// Retry with callback
resp, err := c.Get(ctx, "/api",
    cio.Retry(3, 100, 5000, cio.When5xx()),
    cio.OnRetry(func(attempt int, err error) {
        log.Printf("Retry attempt %d: %v", attempt, err)
    }),
)

// Retry with jitter
resp, err := c.Get(ctx, "/api",
    cio.Retry(3, 100, 5000),
    cio.WithJitter(cio.JitterFull), // or JitterEqual, JitterDecorrelated
)
```

### Status Validation

```go
// Expect 2xx status (error if not)
resp, err := c.Get(ctx, "/api", cio.ExpectOK())
if err != nil {
    var statusErr *cio.StatusError
    if errors.As(err, &statusErr) {
        fmt.Printf("Status: %d, Body: %s\n", statusErr.StatusCode, statusErr.Body)
    }
}

// Expect specific status codes
resp, err := c.Post(ctx, "/users", cio.ExpectStatus(200, 201))
```

### Interceptors

cio provides multiple interceptor hooks for different stages of the request lifecycle:

```go
c := cio.New(
    cio.BaseURL("https://api.example.com"),

    // OnRequest: Modify request before sending
    cio.OnRequest(func(req *http.Request) {
        req.Header.Set("X-Custom", "value")
        log.Printf("-> %s %s", req.Method, req.URL)
    }),

    // OnResponse: Inspect raw response before body is read
    cio.OnResponse(func(resp *http.Response) {
        log.Printf("<- %d %s", resp.StatusCode, resp.Request.URL)
    }),

    // OnAfterRead: Access parsed response after body is read
    cio.OnAfterRead(func(resp *cio.Response, raw *http.Response) {
        log.Printf("Response body size: %d bytes", len(resp.Body))
    }),

    // OnMetrics: Track request metrics (called after request completes)
    cio.OnMetrics(func(method, path string, status int, duration time.Duration, err error) {
        log.Printf("Metrics: %s %s -> %d (%v)", method, path, status, duration)
    }),
)

// Example: Add authentication token to all requests
c := cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.OnRequest(func(req *http.Request) {
        token := getAuthToken()
        req.Header.Set("Authorization", "Bearer "+token)
    }),
)

// Example: Log response headers
c := cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.OnResponse(func(resp *http.Response) {
        log.Printf("Content-Type: %s", resp.Header.Get("Content-Type"))
        log.Printf("Content-Length: %s", resp.Header.Get("Content-Length"))
    }),
)
```

### Cookie Jar

```go
c := cio.New(
    cio.BaseURL("https://example.com"),
    cio.WithCookieJar(),
)

// Cookies are automatically managed
c.Post(ctx, "/login", cio.Body(credentials))
c.Get(ctx, "/profile") // session cookie sent automatically

// Manual cookie management
c.SetCookies("https://example.com", []*http.Cookie{
    {Name: "session", Value: "abc123"},
})

cookies, _ := c.Cookies("https://example.com")
```

### Redirect Control

```go
// Limit redirects
c := cio.New(cio.WithRedirects(3))

// Disable redirects
c := cio.New(cio.DisableRedirects())
resp, _ := c.Get(ctx, "/redirect")
location := resp.Headers.Get("Location")
```

### Rate Limiting

```go
// Create rate limiter (100 requests per second, burst of 10)
rl := cio.NewRateLimiter(100, 10)

// Use with client
c := cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.WithRateLimiter(rl),
)

// Or use shorthand
c := cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.WithRateLimit(100, 10),
)

// Manual rate limiting
if err := rl.Allow(ctx); err != nil {
    // rate limit exceeded or context cancelled
}

// Non-blocking check
if !rl.TryAllow() {
    // rate limit exceeded
}
```

### Circuit Breaker

```go
// Create circuit breaker (5 failures to open, 2 successes to close, 30s timeout)
cb := cio.NewCircuitBreaker(5, 2, 30*time.Second)

// Use with client
c := cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.WithCircuitBreaker(cb),
)

// Or use shorthand
c := cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.WithCircuit(5, 2, 30*time.Second),
)

// Check circuit state
state := cb.State() // CircuitClosed, CircuitOpen, CircuitHalfOpen

// Manual control
cb.RecordSuccess()
cb.RecordFailure()
cb.Reset()
```

### Request ID / Tracing

```go
// Auto request ID
c := cio.New(cio.WithRequestID(func() string {
    return uuid.NewString()
}))

// Built-in tracing (X-Request-ID + X-Service-Name)
c := cio.New(cio.WithTracing("my-service"))

// Detailed timing information
resp, _ := c.Get(ctx, "/api", cio.WithTrace())
fmt.Printf("DNS: %v, Connect: %v, TLS: %v, Total: %v\n",
    resp.Trace.DNSLookup,
    resp.Trace.ConnectTime,
    resp.Trace.TLSHandshake,
    resp.Trace.TotalTime,
)
```

### Caching & Conditional Requests

```go
// Cache control
resp, _ := c.Get(ctx, "/api", cio.DisableCache())
resp, _ := c.Get(ctx, "/api", cio.CacheControl("no-store"))
resp, _ := c.Get(ctx, "/api", cio.CacheControl("max-age=3600"))

// Conditional requests with ETag
resp, _ := c.Get(ctx, "/resource")
etag := resp.ETag()

// Later request
resp, _ = c.Get(ctx, "/resource", cio.IfNoneMatch(etag))
if resp.IsNotModified() {
    // Use cached version
}

// Conditional requests with Last-Modified
resp, _ := c.Get(ctx, "/resource")
lastMod := resp.LastModified()

// Later request
resp, _ = c.Get(ctx, "/resource", cio.IfModifiedSince(lastMod))
if resp.IsNotModified() {
    // Use cached version
}

// Optimistic locking with If-Match
resp, _ = c.Put(ctx, "/resource",
    cio.IfMatch(etag),
    cio.JSONBody(data),
)
```

### Range Requests

```go
// Request specific byte range
resp, _ := c.Get(ctx, "/file", cio.Range(0, 1023)) // First 1KB

// Request from byte to end
resp, _ := c.Get(ctx, "/file", cio.Range(1024, -1)) // From byte 1024 to end

// Check if server supports ranges
if resp.AcceptRanges() {
    // Server supports range requests
}
```

### Compression

```go
// Gzip request body
resp, _ := c.Post(ctx, "/api",
    cio.JSONBody(largeData),
    cio.Gzip(),
)

// Manual response decompression
resp, _ := c.Get(ctx, "/compressed",
    cio.Decompress(),
)
```

### Debug & Dump

```go
// Debug mode (logs request/response summary)
resp, _ := c.Get(ctx, "/api", cio.DebugStdout())
// Output: --> GET https://api.example.com/api
//         <-- 200 OK (123ms)

// Custom debug writer
var buf bytes.Buffer
resp, _ := c.Get(ctx, "/api", cio.Debug(&buf))

// Dump full request
var dump bytes.Buffer
resp, _ := c.Post(ctx, "/api",
    cio.JSONBody(data),
    cio.DumpRequest(&dump),
)

// Dump full response
resp, _ := c.Get(ctx, "/api", cio.DumpResponse(&dump))

// Dump both request and response
resp, _ := c.Post(ctx, "/api",
    cio.JSONBody(data),
    cio.Dump(&dump),
)
```

### Metrics & Monitoring

```go
// Basic metrics logging
c := cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.OnMetrics(func(method, path string, status int, duration time.Duration, err error) {
        log.Printf("%s %s -> %d (%v) err=%v", method, path, status, duration, err)
    }),
)

// Prometheus integration example
var (
    httpRequestsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "http_requests_total",
            Help: "Total number of HTTP requests",
        },
        []string{"method", "path", "status"},
    )
    httpRequestDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "http_request_duration_seconds",
            Help:    "HTTP request latency",
            Buckets: prometheus.DefBuckets,
        },
        []string{"method", "path"},
    )
)

c := cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.OnMetrics(func(method, path string, status int, duration time.Duration, err error) {
        httpRequestsTotal.WithLabelValues(method, path, fmt.Sprintf("%d", status)).Inc()
        httpRequestDuration.WithLabelValues(method, path).Observe(duration.Seconds())
    }),
)

// Multiple metrics interceptors
c := cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.OnMetrics(func(method, path string, status int, duration time.Duration, err error) {
        // Log slow requests
        if duration > 1*time.Second {
            log.Printf("SLOW: %s %s took %v", method, path, duration)
        }
    }),
    cio.OnMetrics(func(method, path string, status int, duration time.Duration, err error) {
        // Track errors
        if err != nil || status >= 500 {
            errorCounter.Inc()
        }
    }),
)
```

### Response Limit

```go
// Limit response body size (prevents memory exhaustion)
resp, err := c.Get(ctx, "/large-file", cio.MaxBodyBytes(10*1024*1024)) // 10MB max
if err != nil {
    var btle *cio.BodyTooLargeError
    if errors.As(err, &btle) {
        log.Printf("Response too large: limit=%d", btle.Limit)
    }
}
```

### Parallel Requests

```go
// Execute multiple requests in parallel
results := c.Parallel(ctx, []cio.ParallelRequest{
    {Method: "GET", Path: "/users"},
    {Method: "GET", Path: "/posts"},
    {Method: "GET", Path: "/comments"},
})

for _, result := range results {
    if result.Error != nil {
        log.Printf("Request %d failed: %v", result.Index, result.Error)
        continue
    }
    log.Printf("Request %d: status=%d", result.Index, result.Response.StatusCode)
}

// Parallel GET shorthand
results := c.ParallelGet(ctx, []string{"/users", "/posts", "/comments"})
```

### Client Cloning

```go
// Create base client
base := cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.WithUserAgent("MyApp/1.0"),
)

// Clone with modifications (shares same connection pool)
authenticated := base.Clone(
    cio.Headers(cio.Bearer("token123")),
)

// Clone with separate connection pool
isolated := base.Clone(
    cio.Transport(&http.Transport{
        MaxIdleConns: 10,
    }),
)
```

### Custom HTTP Client

```go
httpClient := &http.Client{
    Timeout: 30 * time.Second,
    Transport: &http.Transport{
        MaxIdleConns:        100,
        MaxIdleConnsPerHost: 10,
    },
}

c := cio.New(cio.HTTPClient(httpClient))
```

## Content Types

```go
// Application
cio.JSON            // application/json
cio.XML             // application/xml
cio.Form            // application/x-www-form-urlencoded
cio.MultipartForm   // multipart/form-data
cio.OctetStream     // application/octet-stream
cio.PDF             // application/pdf
cio.ZIP             // application/zip
cio.GZIP            // application/gzip
cio.JS              // application/javascript
cio.WASM            // application/wasm
cio.GraphQL         // application/graphql+json
cio.YAML            // application/x-yaml
cio.MsgPack         // application/msgpack
cio.Protobuf        // application/protobuf
cio.CBOR            // application/cbor

// Text
cio.Text            // text/plain
cio.HTML            // text/html
cio.CSS             // text/css
cio.CSV             // text/csv
cio.Markdown        // text/markdown
cio.EventStream     // text/event-stream

// Image
cio.PNG             // image/png
cio.JPEG            // image/jpeg
cio.GIF             // image/gif
cio.WEBP            // image/webp
cio.SVG             // image/svg+xml
cio.ICO             // image/x-icon
cio.AVIF            // image/avif

// Audio
cio.MP3             // audio/mpeg
cio.WAV             // audio/wav
cio.OGG             // audio/ogg
cio.FLAC            // audio/flac
cio.AAC             // audio/aac

// Video
cio.MP4             // video/mp4
cio.WEBM            // video/webm
cio.AVI             // video/x-msvideo

// Font
cio.WOFF            // font/woff
cio.WOFF2           // font/woff2
cio.TTF             // font/ttf
cio.OTF             // font/otf
```

## Complete Example

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "log"
    "net/http"
    "os"

    "github.com/dreamph/cio"
)

type User struct {
    ID   int    `json:"id"`
    Name string `json:"name"`
}

func main() {
    ctx := context.Background()

    // Create client with all features
    c := cio.New(
        cio.BaseURL("https://api.example.com"),
        cio.WithCookieJar(),
        cio.WithRequestID(generateID),
        cio.WithRedirects(5),
        cio.OnRequest(func(req *http.Request) {
            log.Printf("-> %s %s", req.Method, req.URL)
        }),
        cio.OnResponse(func(resp *http.Response) {
            log.Printf("<- %d", resp.StatusCode)
        }),
    )

    // GET with retry and status check
    resp, err := c.Get(ctx, "/users",
        cio.Query("page", "1"),
        cio.Retry(3, 100, cio.When5xx()),
        cio.ExpectOK(),
        cio.Timeout(5000),
    )
    if err != nil {
        var statusErr *cio.StatusError
        if errors.As(err, &statusErr) {
            log.Fatalf("API error %d: %s", statusErr.StatusCode, statusErr.Body)
        }
        log.Fatal(err)
    }

    users, _ := cio.Json[[]User](resp)
    fmt.Println("Users:", users)

    // POST with JSON
    resp, _ = c.Post(ctx, "/users",
        cio.Body(User{Name: "John"}),
        cio.Headers(cio.ContentType(cio.JSON)),
        cio.ExpectStatus(201),
    )

    // Download file
    file, _ := os.Create("report.pdf")
    c.Get(ctx, "/reports/latest.pdf", cio.OutputStream(file))
    file.Close()

    // Upload file
    upload, _ := os.Open("data.csv")
    c.Post(ctx, "/import",
        cio.Files(cio.NewFile("file", "data.csv", upload)),
        cio.FormFields(map[string]string{"type": "users"}),
    )
    upload.Close()
}

func generateID() string {
    // your ID generation logic
    return "req-123"
}
```

## Testing

cio includes comprehensive testing utilities.

### Mock Transport

```go
func TestMyAPI(t *testing.T) {
    mt := cio.NewMockTransport()

    // Register mock responses
    mt.On("GET", "https://api.test/users", cio.MockResponse{
        StatusCode: 200,
        Headers:    http.Header{"Content-Type": []string{"application/json"}},
        Body:       []byte(`[{"id":1,"name":"John"}]`),
    })

    mt.On("POST", "https://api.test/users", cio.MockResponse{
        StatusCode: 201,
        Body:       []byte(`{"id":2,"name":"Jane"}`),
    })

    // Use mock transport
    c := cio.New(
        cio.HTTPClient(&http.Client{Transport: mt}),
        cio.BaseURL("https://api.test"),
    )

    // Make requests
    resp, _ := c.Get(context.Background(), "/users")

    // Verify calls
    calls := mt.Calls()
    if len(calls) != 1 {
        t.Fatalf("expected 1 call, got %d", len(calls))
    }
    if calls[0].Method != "GET" {
        t.Fatalf("expected GET, got %s", calls[0].Method)
    }
}
```

### Record Transport

```go
func TestWithRecording(t *testing.T) {
    rt := cio.NewRecordTransport(http.DefaultTransport)

    c := cio.New(
        cio.HTTPClient(&http.Client{Transport: rt}),
    )

    // Make real requests
    c.Get(context.Background(), "https://api.github.com/users/octocat")

    // Inspect recorded requests/responses
    records := rt.Records()
    for _, rec := range records {
        fmt.Printf("%s %s -> %d (%v)\n",
            rec.Request.Method,
            rec.Request.URL,
            rec.Response.StatusCode,
            rec.Duration,
        )
    }
}
```

### Custom JSON Codec

```go
import "github.com/bytedance/sonic"

c := cio.New(
    cio.WithJSONCodec(sonic.Marshal, sonic.Unmarshal),
)
```

## Best Practices

### 1. Reuse Clients

```go
// Good: Create once, reuse
var apiClient = cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.WithTimeout(30 * time.Second),
)

// Bad: Creating new client for each request
func makeRequest() {
    c := cio.New(...) // Don't do this repeatedly
}
```

### 2. Use Context for Cancellation

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

resp, err := c.Get(ctx, "/api")
```

### 3. Handle Status Errors

```go
resp, err := c.Get(ctx, "/api", cio.ExpectOK())
if err != nil {
    var statusErr *cio.StatusError
    if errors.As(err, &statusErr) {
        log.Printf("API error %d: %s", statusErr.StatusCode, statusErr.Body)
        return
    }
    log.Printf("Request error: %v", err)
}
```

### 4. Use Rate Limiting for Public APIs

```go
c := cio.New(
    cio.BaseURL("https://api.github.com"),
    cio.WithRateLimit(60, 10), // GitHub: 60 req/hour
)
```

### 5. Use Circuit Breaker for Resilience

```go
c := cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.WithCircuit(5, 2, 30*time.Second),
)
```

### 6. Stream Large Files

```go
// Good: Stream to file
file, _ := os.Create("large.zip")
c.Get(ctx, "/files/large.zip", cio.OutputStream(file))
file.Close()

// Bad: Load entire file into memory
resp, _ := c.Get(ctx, "/files/large.zip")
os.WriteFile("large.zip", resp.Body, 0644)
```

## Performance Tips

1. **Connection Pooling**: Use `cio.DefaultTransport()` or customize `MaxIdleConns`, `MaxIdleConnsPerHost`
2. **HTTP/2**: Enabled by default with `ForceAttemptHTTP2: true`
3. **Keep-Alive**: Enabled by default with 90s idle timeout
4. **Buffer Sizes**: Optimized 32KB read/write buffers
5. **Lazy Initialization**: Headers and query maps are lazily allocated

## Error Handling

```go
resp, err := c.Get(ctx, "/api")
if err != nil {
    // Check specific error types
    switch {
    case errors.Is(err, cio.ErrNonRepeatable):
        // Body not repeatable for retry
    case errors.Is(err, cio.ErrCircuitOpen):
        // Circuit breaker is open
    case errors.Is(err, cio.ErrRateLimited):
        // Rate limit exceeded
    case errors.Is(err, context.DeadlineExceeded):
        // Request timeout
    case errors.Is(err, context.Canceled):
        // Request cancelled
    default:
        // Network or other error
    }

    var statusErr *cio.StatusError
    if errors.As(err, &statusErr) {
        // HTTP status error with body
    }

    var bodyErr *cio.BodyTooLargeError
    if errors.As(err, &bodyErr) {
        // Response body too large
    }
}
```

## License

cio is distributed under the MIT License. See `LICENSE` for details.

## Support

[![Buy Me a Coffee](https://www.buymeacoffee.com/assets/img/custom_images/orange_img.png)](https://www.buymeacoffee.com/dreamph)
