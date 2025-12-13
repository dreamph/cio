# cio

A simple, powerful HTTP client library for Go with streaming support, retry, interceptors, and more.

## Installation

```bash
go get github.com/dreamph/cio
```

## Features

- **Simple API** - Clean, intuitive interface
- **Streaming** - Upload/download large files without buffering
- **Multipart Upload** - Easy file uploads with form fields
- **Retry** - Configurable retry with custom conditions
- **Interceptors** - Request/response hooks
- **Cookie Jar** - Automatic cookie management
- **Redirect Control** - Limit or disable redirects
- **Request Tracing** - Built-in request ID support
- **Timeout** - Per-request timeout
- **Status Validation** - Auto error on unexpected status codes

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
// Basic retry (3 attempts, 100ms backoff)
resp, err := c.Get(ctx, "/api", cio.Retry(3, 100))

// Retry on 5xx only
resp, err := c.Get(ctx, "/api", cio.Retry(3, 100, cio.When5xx()))

// Retry on specific status codes
resp, err := c.Get(ctx, "/api", cio.Retry(3, 100, cio.WhenStatus(500, 502, 503, 429)))

// Retry on specific errors
resp, err := c.Get(ctx, "/api", cio.Retry(3, 100, cio.WhenErr(func(err error) bool {
    return errors.Is(err, context.DeadlineExceeded)
})))

// Full custom condition
resp, err := c.Get(ctx, "/api", cio.Retry(3, 100, cio.When(func(resp *cio.Response, err error) bool {
    if err != nil {
        return true // retry on any error
    }
    return resp.StatusCode == 429 || resp.StatusCode >= 500
})))
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

```go
c := cio.New(
    cio.BaseURL("https://api.example.com"),
    cio.OnRequest(func(req *http.Request) {
        req.Header.Set("X-Custom", "value")
        log.Printf("-> %s %s", req.Method, req.URL)
    }),
    cio.OnResponse(func(resp *http.Response) {
        log.Printf("<- %d %s", resp.StatusCode, resp.Request.URL)
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
c := cio.New(cio.NoRedirects())
resp, _ := c.Get(ctx, "/redirect")
location := resp.Headers.Get("Location")
```

### Request ID / Tracing

```go
// Auto request ID
c := cio.New(cio.WithRequestID(func() string {
    return uuid.NewString()
}))

// Built-in tracing (X-Request-ID + X-Service-Name)
c := cio.New(cio.WithTracing("my-service"))
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

## License

 cio is distributed under the MIT License. See `LICENSE` for details.

Buy Me a Coffee
===============
[![](https://www.buymeacoffee.com/assets/img/custom_images/orange_img.png)](https://www.buymeacoffee.com/dreamph)