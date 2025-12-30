package cio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* ========== Response helpers ========== */

func TestResponseHelpers(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", "123")
	h.Set("ETag", `"abc123"`)
	h.Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Location", "https://example.com/redirect")

	r := &Response{
		StatusCode: http.StatusNotModified,
		Headers:    h,
		Body:       []byte(`{"ok":true}`),
	}

	if !r.IsJSON() {
		t.Fatalf("IsJSON should be true")
	}
	if r.IsXML() {
		t.Fatalf("IsXML should be false")
	}
	if r.IsText() {
		t.Fatalf("IsText should be false")
	}
	if !r.IsNotModified() {
		t.Fatalf("IsNotModified should be true")
	}

	if ct := r.ContentType(); ct != "application/json; charset=utf-8" {
		t.Fatalf("ContentType = %q", ct)
	}
	if cl := r.ContentLength(); cl != 123 {
		t.Fatalf("ContentLength = %d", cl)
	}
	if et := r.ETag(); et != `"abc123"` {
		t.Fatalf("ETag = %q", et)
	}
	if !r.LastModified().Equal(time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)) {
		t.Fatalf("LastModified parsed incorrectly: %v", r.LastModified())
	}
	if !r.AcceptRanges() {
		t.Fatalf("AcceptRanges should be true")
	}
	if loc := r.Location(); loc != "https://example.com/redirect" {
		t.Fatalf("Location = %q", loc)
	}
	u, err := r.LocationURL()
	if err != nil {
		t.Fatalf("LocationURL error: %v", err)
	}
	if u.String() != "https://example.com/redirect" {
		t.Fatalf("LocationURL = %q", u.String())
	}
}

func TestResponseCookiesFallback(t *testing.T) {
	h := http.Header{}
	h.Add("Set-Cookie", "foo=bar; Path=/")
	h.Add("Set-Cookie", "baz=qux; Path=/api")
	r := &Response{Headers: h}

	cookies := r.Cookies()
	if len(cookies) != 2 {
		t.Fatalf("expected 2 cookies, got %d", len(cookies))
	}
}

/* ========== JSON helpers ========== */

func TestResponseJsonAndGenericJson(t *testing.T) {
	type Foo struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}

	c := New()
	r := &Response{
		Headers: http.Header{"Content-Type": []string{JSON}},
		Body:    []byte(`{"name":"john","age":30}`),
		client:  c,
	}

	var v Foo
	if err := r.Json(&v); err != nil {
		t.Fatalf("Json error: %v", err)
	}
	if v.Name != "john" || v.Age != 30 {
		t.Fatalf("Json decoded wrong: %+v", v)
	}

	got, err := Json[Foo](r)
	if err != nil {
		t.Fatalf("generic Json error: %v", err)
	}
	if got.Name != "john" || got.Age != 30 {
		t.Fatalf("generic Json decoded wrong: %+v", got)
	}
}

/* ========== buildURL ========== */

func TestBuildURL_BaseAndQuery(t *testing.T) {
	c := New(BaseURL("https://api.example.com/v1"))
	u, err := c.buildURL("users", url.Values{"q": []string{"john"}, "role": []string{"admin"}})
	if err != nil {
		t.Fatalf("buildURL error: %v", err)
	}

	if u != "https://api.example.com/v1/users?q=john&role=admin" &&
		u != "https://api.example.com/v1/users?role=admin&q=john" {
		t.Fatalf("unexpected URL: %s", u)
	}
}

func TestBuildURL_NoBase(t *testing.T) {
	c := New()
	u, err := c.buildURL("https://example.com/path?x=1", url.Values{"y": []string{"2"}})
	if err != nil {
		t.Fatalf("buildURL error: %v", err)
	}
	if !strings.Contains(u, "https://example.com/path") || !strings.Contains(u, "y=2") {
		t.Fatalf("unexpected URL: %s", u)
	}
}

/* ========== read/copy with limit ========== */

func TestReadAllWithLimit_OK(t *testing.T) {
	data := []byte("hello world")
	out, err := readAllWithLimit(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("readAllWithLimit error: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("data mismatch")
	}
}

func TestReadAllWithLimit_TooLarge(t *testing.T) {
	data := []byte("123456")
	_, err := readAllWithLimit(bytes.NewReader(data), 3)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var btle *BodyTooLargeError
	if !errors.As(err, &btle) {
		t.Fatalf("expected BodyTooLargeError, got %T", err)
	}
	if btle.Limit != 3 {
		t.Fatalf("BodyTooLargeError.Limit = %d", btle.Limit)
	}
}

func TestCopyWithLimit_OK(t *testing.T) {
	src := bytes.NewReader([]byte("hello"))
	var dst bytes.Buffer

	n, err := copyWithLimit(&dst, src, 10)
	if err != nil {
		t.Fatalf("copyWithLimit error: %v", err)
	}
	if n != 5 || dst.String() != "hello" {
		t.Fatalf("unexpected copy result n=%d, dst=%q", n, dst.String())
	}
}

func TestCopyWithLimit_TooLarge(t *testing.T) {
	src := bytes.NewReader([]byte("123456"))
	var dst bytes.Buffer

	n, err := copyWithLimit(&dst, src, 3)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var btle *BodyTooLargeError
	if !errors.As(err, &btle) {
		t.Fatalf("expected BodyTooLargeError, got %T", err)
	}
	if n <= 3 {
		t.Fatalf("expected n > limit, got %d", n)
	}
}

/* ========== streamLines / streamRaw ========== */

func TestStreamLines(t *testing.T) {
	data := "line1\nline2\r\n\nlast"
	var lines [][]byte

	err := streamLines(strings.NewReader(data), func(line []byte) error {
		cp := make([]byte, len(line))
		copy(cp, line)
		lines = append(lines, cp)
		return nil
	})
	if err != nil {
		t.Fatalf("streamLines error: %v", err)
	}

	want := []string{"line1", "line2", "", "last"}
	if len(lines) != len(want) {
		t.Fatalf("expected %d lines, got %d", len(want), len(lines))
	}
	for i, w := range want {
		if string(lines[i]) != w {
			t.Fatalf("line %d = %q, want %q", i, lines[i], w)
		}
	}
}

func TestStreamRaw(t *testing.T) {
	data := []byte("abcdefghijklmnopqrstuvwxyz")
	var total []byte

	err := streamRaw(bytes.NewReader(data), func(chunk []byte) error {
		total = append(total, chunk...)
		return nil
	})
	if err != nil {
		t.Fatalf("streamRaw error: %v", err)
	}
	if !bytes.Equal(total, data) {
		t.Fatalf("streamRaw data mismatch")
	}
}

/* ========== gzip helpers ========== */

func TestGzipBodyAndDecompressReader(t *testing.T) {
	original := []byte("hello gzip world")

	factory := func() (io.ReadCloser, string, int64, error) {
		return io.NopCloser(bytes.NewReader(original)), "text/plain", int64(len(original)), nil
	}

	gzFactory := gzipBody(factory)
	rc, ct, n, err := gzFactory()
	if err != nil {
		t.Fatalf("gzipBody factory error: %v", err)
	}
	defer rc.Close()

	if ct != "text/plain" {
		t.Fatalf("content type should be preserved, got %q", ct)
	}
	if n != -1 {
		t.Fatalf("content length for gzip body should be -1, got %d", n)
	}

	// Read compressed
	compressed, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read compressed error: %v", err)
	}
	if len(compressed) == 0 {
		t.Fatalf("compressed data is empty")
	}

	// Decompress using decompressReader
	dr, err := decompressReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("decompressReader error: %v", err)
	}
	defer dr.Close()

	decoded, err := io.ReadAll(dr)
	if err != nil {
		t.Fatalf("read decompressed error: %v", err)
	}
	if !bytes.Equal(decoded, original) {
		t.Fatalf("decompressed mismatch: got %q, want %q", decoded, original)
	}
}

/* ========== RateLimiter ========== */

func TestRateLimiterTryAllow(t *testing.T) {
	rl := NewRateLimiter(1, 1) // 1 token, rate 1/s
	if !rl.TryAllow() {
		t.Fatalf("first TryAllow should pass")
	}
	if rl.TryAllow() {
		t.Fatalf("second TryAllow should fail immediately")
	}
}

func TestRateLimiterAllowFast(t *testing.T) {
	rl := NewRateLimiter(1000, 1)
	ctx := context.Background()
	start := time.Now()
	if err := rl.Allow(ctx); err != nil {
		t.Fatalf("Allow error: %v", err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatalf("Allow took too long")
	}
}

/* ========== CircuitBreaker ========== */

func TestCircuitBreakerTransitions(t *testing.T) {
	cb := NewCircuitBreaker(2, 2, 50*time.Millisecond)

	// Initially closed
	if cb.State() != CircuitClosed {
		t.Fatalf("initial state should be closed")
	}

	// Two failures -> open
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.State() != CircuitOpen {
		t.Fatalf("state should be open")
	}

	// While open and before timeout -> ErrCircuitOpen
	if err := cb.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Allow should return ErrCircuitOpen, got %v", err)
	}

	// Wait timeout -> half-open
	time.Sleep(60 * time.Millisecond)
	if err := cb.Allow(); err != nil {
		t.Fatalf("Allow after timeout should be nil, got %v", err)
	}
	if cb.State() != CircuitHalfOpen {
		t.Fatalf("state should be half-open")
	}

	// Two successes -> closed
	cb.RecordSuccess()
	cb.RecordSuccess()
	if cb.State() != CircuitClosed {
		t.Fatalf("state should be closed")
	}
}

/* ========== Retry + non-repeatable body ========== */

type nonSeekReader struct {
	data []byte
	pos  int
}

func (r *nonSeekReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func TestRetryOnNonRepeatableBodyReturnsErrNonRepeatable(t *testing.T) {
	c := New(BaseURL("https://example.com"))

	ctx := context.Background()
	_, err := c.Post(ctx, "/test",
		BodyReader(&nonSeekReader{data: []byte("hello")}),
		Retry(1, 10, 100),
	)
	if !errors.Is(err, ErrNonRepeatable) {
		t.Fatalf("expected ErrNonRepeatable, got %v", err)
	}
}

/* ========== MockTransport + ExpectStatus ========== */

func TestClientWithMockTransportAndExpectOK(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/ok", MockResponse{
		StatusCode: 200,
		Body:       []byte(`{"ok":true}`),
	})
	mt.On("GET", "https://api.test/notfound", MockResponse{
		StatusCode: 404,
		Body:       []byte(`{"error":"not found"}`),
	})

	httpClient := &http.Client{Transport: mt}
	c := New(
		HTTPClient(httpClient),
		BaseURL("https://api.test"),
	)

	ctx := context.Background()

	// OK case
	resp, err := c.Get(ctx, "/ok", ExpectOK())
	if err != nil {
		t.Fatalf("Get /ok error: %v", err)
	}
	if !resp.OK() {
		t.Fatalf("resp.OK should be true")
	}

	// Not found with ExpectOK should return StatusError
	resp, err = c.Get(ctx, "/notfound", ExpectOK())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("expected StatusError, got %T", err)
	}
	if se.StatusCode != 404 {
		t.Fatalf("StatusError.StatusCode = %d", se.StatusCode)
	}
	if se.Method != http.MethodGet || se.URL != "https://api.test/notfound" {
		t.Fatalf("StatusError method/url mismatch: %s %s", se.Method, se.URL)
	}
}

/* ========== Metrics interceptor ========== */

func TestOnMetricsCalled(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.metrics/test", MockResponse{
		StatusCode: 200,
		Body:       []byte("ok"),
	})
	httpClient := &http.Client{Transport: mt}

	var called bool
	var gotMethod, gotPath string
	var gotStatus int
	var gotErr error

	c := New(
		HTTPClient(httpClient),
		BaseURL("https://api.metrics"),
		OnMetrics(func(method, path string, status int, duration time.Duration, err error) {
			called = true
			gotMethod = method
			gotPath = path
			gotStatus = status
			gotErr = err
		}),
	)

	ctx := context.Background()
	_, err := c.Get(ctx, "/test")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}

	if !called {
		t.Fatalf("metrics interceptor not called")
	}
	if gotMethod != http.MethodGet || gotPath != "/test" || gotStatus != 200 || gotErr != nil {
		t.Fatalf("metrics values incorrect: %s %s %d %v", gotMethod, gotPath, gotStatus, gotErr)
	}
}

/* ========== TraceInfo ========== */

func TestWithTracePopulatesTraceInfo(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.trace/trace", MockResponse{
		StatusCode: 200,
		Body:       []byte("ok"),
	})
	httpClient := &http.Client{Transport: mt}

	c := New(
		HTTPClient(httpClient),
		BaseURL("https://api.trace"),
	)

	ctx := context.Background()
	resp, err := c.Get(ctx, "/trace", WithTrace())
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if resp.Trace == nil {
		t.Fatalf("TraceInfo should not be nil when WithTrace enabled")
	}
	if resp.Trace.TotalTime <= 0 {
		t.Fatalf("TotalTime should be > 0")
	}
}

/* ========== Download / Upload ========== */

func TestDownloadToFile(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://files.test/file", MockResponse{
		StatusCode: 200,
		Body:       []byte("FILEDATA"),
	})
	httpClient := &http.Client{Transport: mt}

	c := New(
		HTTPClient(httpClient),
		BaseURL("https://files.test"),
	)

	dir := t.TempDir()
	dst := filepath.Join(dir, "out.bin")

	ctx := context.Background()
	resp, err := c.Download(ctx, "/file", dst)
	if err != nil {
		t.Fatalf("Download error: %v", err)
	}
	if !resp.OK() {
		t.Fatalf("Download response not OK: %d", resp.StatusCode)
	}

	content, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read downloaded file error: %v", err)
	}
	if string(content) != "FILEDATA" {
		t.Fatalf("downloaded data mismatch: %q", string(content))
	}
}

func TestUploadFile(t *testing.T) {
	mt := NewMockTransport()
	mt.On("POST", "https://files.test/upload", MockResponse{
		StatusCode: 200,
		Body:       []byte("OK"),
	})
	httpClient := &http.Client{Transport: mt}

	c := New(
		HTTPClient(httpClient),
		BaseURL("https://files.test"),
	)

	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("hello upload"), 0o644); err != nil {
		t.Fatalf("write src file error: %v", err)
	}

	ctx := context.Background()
	resp, err := c.Upload(ctx, "/upload", src, "file")
	if err != nil {
		t.Fatalf("Upload error: %v", err)
	}
	if !resp.OK() {
		t.Fatalf("upload resp not OK: %d", resp.StatusCode)
	}

	calls := mt.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	call := calls[0]
	ct := call.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data;") {
		t.Fatalf("Content-Type should be multipart/form-data, got %q", ct)
	}
	if !strings.Contains(string(call.Body), "hello upload") {
		t.Fatalf("multipart body does not contain file content")
	}
}

/* ========== RecordTransport ========== */

func TestRecordTransportRecordsRequests(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://rec.test/path", MockResponse{
		StatusCode: 200,
		Body:       []byte("OK"),
	})

	rt := NewRecordTransport(mt)
	httpClient := &http.Client{Transport: rt}

	c := New(
		HTTPClient(httpClient),
		BaseURL("https://rec.test"),
	)

	ctx := context.Background()
	_, err := c.Get(ctx, "/path")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}

	recs := rt.Records()
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	if string(recs[0].RespBody) != "OK" {
		t.Fatalf("RespBody = %q", string(recs[0].RespBody))
	}
}

/* ========== Parallel ========== */

func TestParallelRequests(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://par.test/a", MockResponse{StatusCode: 200, Body: []byte("A")})
	mt.On("GET", "https://par.test/b", MockResponse{StatusCode: 200, Body: []byte("B")})

	httpClient := &http.Client{Transport: mt}
	c := New(
		HTTPClient(httpClient),
		BaseURL("https://par.test"),
	)

	ctx := context.Background()
	results := c.ParallelGet(ctx, []string{"/a", "/b"})

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	seen := map[string]bool{}
	for _, r := range results {
		if r.Error != nil {
			t.Fatalf("Parallel error: %v", r.Error)
		}
		seen[string(r.Response.Body)] = true
	}
	if !(seen["A"] && seen["B"]) {
		t.Fatalf("unexpected bodies: %+v", seen)
	}
}

/* ========== stream OnStream / OnStreamRaw integration ========== */

func TestOnStreamIntegration(t *testing.T) {
	// direct test via streamLines already; here just ensure OnStream path returns Response
	mt := NewMockTransport()
	mt.On("GET", "https://stream.test/sse", MockResponse{
		StatusCode: 200,
		Body:       []byte("line1\nline2\n"),
	})
	httpClient := &http.Client{Transport: mt}
	c := New(
		HTTPClient(httpClient),
		BaseURL("https://stream.test"),
	)

	var lines []string
	ctx := context.Background()
	resp, err := c.Get(ctx, "/sse", OnStream(func(line []byte) error {
		lines = append(lines, string(line))
		return nil
	}))
	if err != nil {
		t.Fatalf("Get with OnStream error: %v", err)
	}
	if resp == nil {
		t.Fatalf("Response should not be nil")
	}
	if len(lines) != 2 || lines[0] != "line1" || lines[1] != "line2" {
		t.Fatalf("lines = %+v", lines)
	}
}

func TestOnStreamRawIntegration(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://stream.test/raw", MockResponse{
		StatusCode: 200,
		Body:       []byte("chunk-data"),
	})
	httpClient := &http.Client{Transport: mt}
	c := New(
		HTTPClient(httpClient),
		BaseURL("https://stream.test"),
	)

	var buf bytes.Buffer
	ctx := context.Background()
	resp, err := c.Get(ctx, "/raw", OnStreamRaw(func(chunk []byte) error {
		buf.Write(chunk)
		return nil
	}))
	if err != nil {
		t.Fatalf("Get with OnStreamRaw error: %v", err)
	}
	if resp == nil {
		t.Fatalf("Response should not be nil")
	}
	if buf.String() != "chunk-data" {
		t.Fatalf("unexpected stream raw data: %q", buf.String())
	}
}

/* ========== Client Options ========== */

func TestClientWithDefaultHeaders(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200, Body: []byte("OK")})

	c := New(
		HTTPClient(&http.Client{Transport: mt}),
		BaseURL("https://api.test"),
		WithDefaultHeaders(map[string]string{
			"X-Custom": "value",
			"X-Tenant": "test",
		}),
	)

	ctx := context.Background()
	_, err := c.Get(ctx, "/path")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}

	calls := mt.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Header.Get("X-Custom") != "value" {
		t.Fatalf("X-Custom header missing")
	}
	if calls[0].Header.Get("X-Tenant") != "test" {
		t.Fatalf("X-Tenant header missing")
	}
}

func TestClientWithUserAgent(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(
		HTTPClient(&http.Client{Transport: mt}),
		BaseURL("https://api.test"),
		WithUserAgent("MyApp/1.0"),
	)

	ctx := context.Background()
	c.Get(ctx, "/path")

	calls := mt.Calls()
	if calls[0].Header.Get("User-Agent") != "MyApp/1.0" {
		t.Fatalf("User-Agent header = %q", calls[0].Header.Get("User-Agent"))
	}
}

func TestClientWithBasicAuth(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(
		HTTPClient(&http.Client{Transport: mt}),
		BaseURL("https://api.test"),
		WithBasicAuth("user", "pass"),
	)

	ctx := context.Background()
	c.Get(ctx, "/path")

	calls := mt.Calls()
	auth := calls[0].Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Basic ") {
		t.Fatalf("Authorization header = %q", auth)
	}
}

func TestClientWithTimeout(t *testing.T) {
	c := New(WithTimeout(5 * time.Second))
	if c.defaultTimeout != 5*time.Second {
		t.Fatalf("defaultTimeout = %v", c.defaultTimeout)
	}
}

func TestClientClone(t *testing.T) {
	c1 := New(
		BaseURL("https://api.test"),
		WithUserAgent("Test/1.0"),
		WithDefaultHeaders(map[string]string{"X-Original": "value"}),
	)

	c2 := c1.Clone(
		WithUserAgent("Test/2.0"),
	)

	if c2.baseURL != c1.baseURL {
		t.Fatalf("baseURL not cloned")
	}
	if c2.userAgent != "Test/2.0" {
		t.Fatalf("userAgent not overridden")
	}
	if c2.defaultHeaders["X-Original"] != "value" {
		t.Fatalf("defaultHeaders not cloned")
	}
}

func TestClientCloseIdleConnections(t *testing.T) {
	c := New()
	// Should not panic
	c.CloseIdleConnections()
}

func TestClientSetCookiesError(t *testing.T) {
	c := New() // no cookie jar
	err := c.SetCookies("https://example.com", []*http.Cookie{{Name: "test", Value: "val"}})
	if !errors.Is(err, ErrNoCookieJar) {
		t.Fatalf("expected ErrNoCookieJar, got %v", err)
	}
}

func TestClientCookiesError(t *testing.T) {
	c := New() // no cookie jar
	_, err := c.Cookies("https://example.com")
	if !errors.Is(err, ErrNoCookieJar) {
		t.Fatalf("expected ErrNoCookieJar, got %v", err)
	}
}

func TestClientCookieJarSuccess(t *testing.T) {
	c := New(WithCookieJar())
	err := c.SetCookies("https://example.com", []*http.Cookie{{Name: "test", Value: "val"}})
	if err != nil {
		t.Fatalf("SetCookies error: %v", err)
	}

	cookies, err := c.Cookies("https://example.com")
	if err != nil {
		t.Fatalf("Cookies error: %v", err)
	}
	if len(cookies) != 1 || cookies[0].Name != "test" {
		t.Fatalf("cookies = %+v", cookies)
	}
}

/* ========== Request Options ========== */

func TestBodyBytes(t *testing.T) {
	mt := NewMockTransport()
	mt.On("POST", "https://api.test/data", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Post(ctx, "/data", BodyBytes([]byte("raw bytes")))

	calls := mt.Calls()
	if string(calls[0].Body) != "raw bytes" {
		t.Fatalf("body = %q", string(calls[0].Body))
	}
}

func TestBodyReader(t *testing.T) {
	mt := NewMockTransport()
	mt.On("POST", "https://api.test/data", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Post(ctx, "/data", BodyReader(strings.NewReader("from reader")))

	calls := mt.Calls()
	if string(calls[0].Body) != "from reader" {
		t.Fatalf("body = %q", string(calls[0].Body))
	}
}

func TestFormURLEncoded(t *testing.T) {
	mt := NewMockTransport()
	mt.On("POST", "https://api.test/form", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Post(ctx, "/form", FormURLEncoded(map[string]string{
		"username": "john",
		"password": "secret",
	}))

	calls := mt.Calls()
	if calls[0].Header.Get("Content-Type") != Form {
		t.Fatalf("Content-Type = %q", calls[0].Header.Get("Content-Type"))
	}
	body := string(calls[0].Body)
	if !strings.Contains(body, "username=john") || !strings.Contains(body, "password=secret") {
		t.Fatalf("body = %q", body)
	}
}

func TestFormURLEncodedValues(t *testing.T) {
	mt := NewMockTransport()
	mt.On("POST", "https://api.test/form", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()

	v := url.Values{}
	v.Add("key", "val1")
	v.Add("key", "val2")

	c.Post(ctx, "/form", FormURLEncodedValues(v))

	calls := mt.Calls()
	body := string(calls[0].Body)
	if !strings.Contains(body, "key=val1") || !strings.Contains(body, "key=val2") {
		t.Fatalf("body = %q", body)
	}
}

func TestXMLBody(t *testing.T) {
	mt := NewMockTransport()
	mt.On("POST", "https://api.test/xml", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Post(ctx, "/xml", XMLBody([]byte("<root>data</root>")))

	calls := mt.Calls()
	if calls[0].Header.Get("Content-Type") != XML {
		t.Fatalf("Content-Type = %q", calls[0].Header.Get("Content-Type"))
	}
	if string(calls[0].Body) != "<root>data</root>" {
		t.Fatalf("body = %q", string(calls[0].Body))
	}
}

func TestBasicAuthOption(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Get(ctx, "/path", BasicAuth("user", "pass"))

	calls := mt.Calls()
	auth := calls[0].Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Basic ") {
		t.Fatalf("Authorization header = %q", auth)
	}
}

func TestHostHeaderOption(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Get(ctx, "/path", HostHeader("custom.host"))

	// Note: Host header override is set on req.Host, not in headers
	// This test ensures no panic
}

func TestQuerySetAndAdd(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Get(ctx, "/path",
		QuerySet("key", "val1"),
		QueryAdd("key", "val2"),
	)

	calls := mt.Calls()
	u := calls[0].URL
	if !strings.Contains(u, "key=val1") || !strings.Contains(u, "key=val2") {
		t.Fatalf("URL = %q", u)
	}
}

func TestQueryValues(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()

	v := url.Values{}
	v.Add("page", "1")
	v.Add("limit", "10")

	c.Get(ctx, "/path", QueryValues(v))

	calls := mt.Calls()
	u := calls[0].URL
	if !strings.Contains(u, "page=1") || !strings.Contains(u, "limit=10") {
		t.Fatalf("URL = %q", u)
	}
}

func TestRangeOption(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/file", MockResponse{StatusCode: 206})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Get(ctx, "/file", Range(0, 999))

	calls := mt.Calls()
	if calls[0].Header.Get("Range") != "bytes=0-999" {
		t.Fatalf("Range header = %q", calls[0].Header.Get("Range"))
	}
}

func TestRangeOptionOpenEnded(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/file", MockResponse{StatusCode: 206})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Get(ctx, "/file", Range(1000, -1))

	calls := mt.Calls()
	if calls[0].Header.Get("Range") != "bytes=1000-" {
		t.Fatalf("Range header = %q", calls[0].Header.Get("Range"))
	}
}

func TestIfNoneMatch(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/resource", MockResponse{StatusCode: 304})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Get(ctx, "/resource", IfNoneMatch(`"abc123"`))

	calls := mt.Calls()
	if calls[0].Header.Get("If-None-Match") != `"abc123"` {
		t.Fatalf("If-None-Match header = %q", calls[0].Header.Get("If-None-Match"))
	}
}

func TestIfModifiedSince(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/resource", MockResponse{StatusCode: 304})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	c.Get(ctx, "/resource", IfModifiedSince(ts))

	calls := mt.Calls()
	header := calls[0].Header.Get("If-Modified-Since")
	if header == "" {
		t.Fatalf("If-Modified-Since header missing")
	}
}

func TestIfMatch(t *testing.T) {
	mt := NewMockTransport()
	mt.On("PUT", "https://api.test/resource", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Put(ctx, "/resource", IfMatch(`"xyz"`), BodyBytes([]byte("data")))

	calls := mt.Calls()
	if calls[0].Header.Get("If-Match") != `"xyz"` {
		t.Fatalf("If-Match header = %q", calls[0].Header.Get("If-Match"))
	}
}

func TestCacheControlOptions(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Get(ctx, "/path", DisableCache())

	calls := mt.Calls()
	if calls[0].Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("Cache-Control header = %q", calls[0].Header.Get("Cache-Control"))
	}
}

func TestNoCacheAndNoStore(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/a", MockResponse{StatusCode: 200})
	mt.On("GET", "https://api.test/b", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()

	c.Get(ctx, "/a", DisableCache())
	c.Get(ctx, "/b", CacheControl("no-store"))

	calls := mt.Calls()
	if calls[0].Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("DisableCache not working")
	}
	if calls[1].Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("NoStore not working")
	}
}

func TestWithRawCapture(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200, Body: []byte("OK")})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	resp, err := c.Get(ctx, "/path", WithRawCapture())
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if resp.RawRequest == nil || resp.RawResponse == nil {
		t.Fatalf("RawRequest/RawResponse should not be nil")
	}
}

func TestDebugStdout(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	// Should not panic
	c.Get(ctx, "/path", DebugStdout())
}

func TestDebugCustomWriter(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	var buf bytes.Buffer
	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Get(ctx, "/path", Debug(&buf))

	output := buf.String()
	if !strings.Contains(output, "GET") || !strings.Contains(output, "200") {
		t.Fatalf("debug output = %q", output)
	}
}

func TestDumpRequestAndResponse(t *testing.T) {
	mt := NewMockTransport()
	mt.On("POST", "https://api.test/path", MockResponse{
		StatusCode: 200,
		Body:       []byte("response body"),
	})

	var buf bytes.Buffer
	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Post(ctx, "/path", BodyBytes([]byte("request body")), Dump(&buf))

	output := buf.String()
	if !strings.Contains(output, "POST") || !strings.Contains(output, "request body") ||
		!strings.Contains(output, "response body") {
		t.Fatalf("dump output missing expected content")
	}
}

func TestMaxBodyBytes(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/large", MockResponse{
		StatusCode: 200,
		Body:       []byte("123456789012345"),
	})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	_, err := c.Get(ctx, "/large", MaxBodyBytes(5))

	if err == nil {
		t.Fatalf("expected error for body too large")
	}
	var btle *BodyTooLargeError
	if !errors.As(err, &btle) {
		t.Fatalf("expected BodyTooLargeError, got %T", err)
	}
}

func TestOnRetryCallback(t *testing.T) {
	mt := NewMockTransport()
	mt.OnAny(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("network error")
	})

	var attempts []int
	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	c.Get(ctx, "/path",
		Retry(2, 1, 10),
		OnRetry(func(attempt int, err error) {
			attempts = append(attempts, attempt)
		}),
	)

	if len(attempts) != 2 {
		t.Fatalf("OnRetry called %d times, expected 2", len(attempts))
	}
	if attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("attempts = %v", attempts)
	}
}

func TestWhenErrRetryCondition(t *testing.T) {
	mt := NewMockTransport()
	callCount := 0
	mt.OnAny(func(req *http.Request) (*http.Response, error) {
		callCount++
		if callCount == 1 {
			return nil, context.DeadlineExceeded
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewReader([]byte("OK"))),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	resp, err := c.Get(ctx, "/path",
		Retry(1, 1, 10, WhenErr(func(err error) bool {
			return errors.Is(err, context.DeadlineExceeded)
		})),
	)

	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if callCount != 2 {
		t.Fatalf("callCount = %d, expected 2", callCount)
	}
}

/* ========== HTTP Methods ========== */

func TestAllHTTPMethods(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})
	mt.On("POST", "https://api.test/path", MockResponse{StatusCode: 201})
	mt.On("PUT", "https://api.test/path", MockResponse{StatusCode: 200})
	mt.On("PATCH", "https://api.test/path", MockResponse{StatusCode: 200})
	mt.On("DELETE", "https://api.test/path", MockResponse{StatusCode: 204})
	mt.On("HEAD", "https://api.test/path", MockResponse{StatusCode: 200})
	mt.On("OPTIONS", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()

	methods := []struct {
		name   string
		fn     func() (*Response, error)
		status int
	}{
		{"GET", func() (*Response, error) { return c.Get(ctx, "/path") }, 200},
		{"POST", func() (*Response, error) { return c.Post(ctx, "/path") }, 201},
		{"PUT", func() (*Response, error) { return c.Put(ctx, "/path") }, 200},
		{"PATCH", func() (*Response, error) { return c.Patch(ctx, "/path") }, 200},
		{"DELETE", func() (*Response, error) { return c.Delete(ctx, "/path") }, 204},
		{"HEAD", func() (*Response, error) { return c.Head(ctx, "/path") }, 200},
		{"OPTIONS", func() (*Response, error) { return c.Options(ctx, "/path") }, 200},
	}

	for _, m := range methods {
		resp, err := m.fn()
		if err != nil {
			t.Fatalf("%s error: %v", m.name, err)
		}
		if resp.StatusCode != m.status {
			t.Fatalf("%s status = %d, want %d", m.name, resp.StatusCode, m.status)
		}
	}
}

func TestDo(t *testing.T) {
	mt := NewMockTransport()
	mt.On("CUSTOM", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	resp, err := c.Do(ctx, "CUSTOM", "/path")
	if err != nil {
		t.Fatalf("Do error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

/* ========== Context Cancellation ========== */

func TestContextCancellation(t *testing.T) {
	mt := NewMockTransport()
	mt.OnAny(func(req *http.Request) (*http.Response, error) {
		// Check if context is already cancelled
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		default:
		}
		time.Sleep(100 * time.Millisecond)
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(bytes.NewReader([]byte("OK"))),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := c.Get(ctx, "/path")
	if err == nil {
		t.Fatalf("expected error for cancelled context")
	}
}

/* ========== Rate Limiter Advanced ========== */

func TestRateLimiterContextCancellation(t *testing.T) {
	rl := NewRateLimiter(0.1, 1) // very slow rate
	rl.TryAllow()                // consume the only token

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := rl.Allow(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

/* ========== Circuit Breaker Advanced ========== */

func TestCircuitBreakerReset(t *testing.T) {
	cb := NewCircuitBreaker(2, 2, 100*time.Millisecond)
	cb.RecordFailure()
	cb.RecordFailure()

	if cb.State() != CircuitOpen {
		t.Fatalf("should be open")
	}

	cb.Reset()

	if cb.State() != CircuitClosed {
		t.Fatalf("should be closed after reset")
	}
}

func TestCircuitBreakerHalfOpenFailure(t *testing.T) {
	cb := NewCircuitBreaker(1, 2, 50*time.Millisecond)
	cb.RecordFailure()

	if cb.State() != CircuitOpen {
		t.Fatalf("should be open")
	}

	time.Sleep(60 * time.Millisecond)
	cb.Allow() // transitions to half-open

	if cb.State() != CircuitHalfOpen {
		t.Fatalf("should be half-open")
	}

	cb.RecordFailure() // failure in half-open -> back to open

	if cb.State() != CircuitOpen {
		t.Fatalf("should be open again")
	}
}

/* ========== Client with Rate Limiter and Circuit Breaker ========== */

func TestClientWithRateLimiter(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	rl := NewRateLimiter(100, 2)
	c := New(
		HTTPClient(&http.Client{Transport: mt}),
		BaseURL("https://api.test"),
		WithRateLimiter(rl),
	)

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, err := c.Get(ctx, "/path")
		if err != nil {
			t.Fatalf("Get %d error: %v", i, err)
		}
	}
}

func TestClientWithRateLimit(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(
		HTTPClient(&http.Client{Transport: mt}),
		BaseURL("https://api.test"),
		WithRateLimit(100, 2),
	)

	ctx := context.Background()
	_, err := c.Get(ctx, "/path")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
}

func TestClientWithCircuitBreaker(t *testing.T) {
	mt := NewMockTransport()
	mt.OnAny(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("network error")
	})

	cb := NewCircuitBreaker(2, 2, 100*time.Millisecond)
	c := New(
		HTTPClient(&http.Client{Transport: mt}),
		BaseURL("https://api.test"),
		WithCircuitBreaker(cb),
	)

	ctx := context.Background()

	// First two requests fail, circuit opens
	c.Get(ctx, "/path")
	c.Get(ctx, "/path")

	// Third request should fail with ErrCircuitOpen
	_, err := c.Get(ctx, "/path")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
}

func TestClientWithCircuit(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(
		HTTPClient(&http.Client{Transport: mt}),
		BaseURL("https://api.test"),
		WithCircuit(5, 2, 100*time.Millisecond),
	)

	ctx := context.Background()
	_, err := c.Get(ctx, "/path")
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
}

/* ========== Multipart with Context Cancellation ========== */

func TestMultipartWithContextCancellation(t *testing.T) {
	mt := NewMockTransport()
	mt.OnAny(func(req *http.Request) (*http.Response, error) {
		// Simulate slow response
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(200 * time.Millisecond):
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(bytes.NewReader([]byte("OK"))),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}
	})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	largeData := bytes.Repeat([]byte("x"), 1024) // 1KB (smaller for faster test)
	_, err := c.Post(ctx, "/upload",
		Files(NewFile("file", "large.bin", bytes.NewReader(largeData))),
	)

	// Should timeout or be cancelled
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}

/* ========== Response Context ========== */

func TestResponseContext(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	resp, _ := c.Get(ctx, "/path")

	if resp.Context() == nil {
		t.Fatalf("Response.Context() should not be nil")
	}
}

/* ========== StatusError String ========== */

func TestStatusErrorString(t *testing.T) {
	se := &StatusError{
		StatusCode: 404,
		Status:     "Not Found",
		Method:     "GET",
		URL:        "https://api.test/missing",
		Body:       []byte("not found"),
	}

	s := se.Error()
	if !strings.Contains(s, "404") || !strings.Contains(s, "GET") || !strings.Contains(s, "https://api.test/missing") {
		t.Fatalf("StatusError.Error() = %q", s)
	}
}

func TestStatusErrorStringWithoutMethodURL(t *testing.T) {
	se := &StatusError{
		StatusCode: 500,
		Status:     "Internal Server Error",
	}

	s := se.Error()
	if !strings.Contains(s, "500") || !strings.Contains(s, "Internal Server Error") {
		t.Fatalf("StatusError.Error() = %q", s)
	}
}

/* ========== OnStream with ErrStopStream ========== */

func TestOnStreamWithErrStopStream(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/stream", MockResponse{
		StatusCode: 200,
		Body:       []byte("line1\nline2\nline3\n"),
	})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()

	var lines []string
	_, err := c.Get(ctx, "/stream", OnStream(func(line []byte) error {
		lines = append(lines, string(line))
		if len(lines) == 2 {
			return ErrStopStream
		}
		return nil
	}))

	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
}

func TestOnStreamRawWithErrStopStream(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/stream", MockResponse{
		StatusCode: 200,
		Body:       []byte("chunk1chunk2chunk3"),
	})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()

	chunkCount := 0
	_, err := c.Get(ctx, "/stream", OnStreamRaw(func(chunk []byte) error {
		chunkCount++
		if chunkCount == 1 {
			return ErrStopStream
		}
		return nil
	}))

	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if chunkCount != 1 {
		t.Fatalf("expected 1 chunk, got %d", chunkCount)
	}
}

/* ========== OutputStream Integration ========== */

func TestOutputStreamWithMaxBodyBytes(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/file", MockResponse{
		StatusCode: 200,
		Body:       []byte("123456789012345"),
	})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()

	var buf bytes.Buffer
	_, err := c.Get(ctx, "/file", OutputStream(&buf), MaxBodyBytes(5))

	if err == nil {
		t.Fatalf("expected error for body too large")
	}
}

/* ========== Gzip Request/Response ========== */

func TestGzipRequestOption(t *testing.T) {
	mt := NewMockTransport()
	mt.On("POST", "https://api.test/data", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()
	// Use larger data to ensure compression
	largeData := bytes.Repeat([]byte("test data "), 100)
	c.Post(ctx, "/data", BodyBytes(largeData), Gzip())

	calls := mt.Calls()
	if calls[0].Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding should be gzip")
	}
	// With gzip, compressed data should be smaller than original for repetitive data
	if len(calls[0].Body) >= len(largeData) {
		t.Fatalf("body should be compressed: got %d bytes, original %d bytes", len(calls[0].Body), len(largeData))
	}
}

/* ========== R Struct Option ========== */

func TestRStructOption(t *testing.T) {
	mt := NewMockTransport()
	mt.On("POST", "https://api.test/api", MockResponse{StatusCode: 200})

	c := New(HTTPClient(&http.Client{Transport: mt}), BaseURL("https://api.test"))
	ctx := context.Background()

	type Payload struct {
		Name string `json:"name"`
	}

	c.Post(ctx, "/api", R{
		Timeout: 5 * time.Second,
		Headers: map[string]string{"X-Custom": "value"},
		Query:   map[string]string{"key": "val"},
		Body:    Payload{Name: "test"},
	})

	calls := mt.Calls()
	if calls[0].Header.Get("X-Custom") != "value" {
		t.Fatalf("X-Custom header missing")
	}
	if !strings.Contains(calls[0].URL, "key=val") {
		t.Fatalf("query param missing")
	}
	if !strings.Contains(string(calls[0].Body), "test") {
		t.Fatalf("body missing")
	}
}

/* ========== Transport Option ========== */

func TestTransportOption(t *testing.T) {
	customTransport := &http.Transport{
		MaxIdleConns: 50,
	}

	c := New(Transport(customTransport))
	if c.http.Transport != customTransport {
		t.Fatalf("custom transport not set")
	}
}

/* ========== DefaultTransport ========== */

func TestDefaultTransport(t *testing.T) {
	tr := DefaultTransport()
	if tr.MaxIdleConns != 100 {
		t.Fatalf("DefaultTransport.MaxIdleConns = %d", tr.MaxIdleConns)
	}
}

/* ========== WithTracing ========== */

func TestWithTracing(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200})

	c := New(
		HTTPClient(&http.Client{Transport: mt}),
		BaseURL("https://api.test"),
		WithTracing("my-service"),
	)

	ctx := context.Background()
	c.Get(ctx, "/path")

	calls := mt.Calls()
	if calls[0].Header.Get("X-Service-Name") != "my-service" {
		t.Fatalf("X-Service-Name header missing")
	}
	if calls[0].Header.Get("X-Request-Id") == "" {
		t.Fatalf("X-Request-Id header missing")
	}
}

/* ========== WithRedirects ========== */

func TestWithRedirects(t *testing.T) {
	c := New(WithRedirects(3))
	if c.http.CheckRedirect == nil {
		t.Fatalf("CheckRedirect should be set")
	}
}

func TestDisableRedirects(t *testing.T) {
	c := New(DisableRedirects())
	if c.http.CheckRedirect == nil {
		t.Fatalf("CheckRedirect should be set")
	}
}

/* ========== OnAfterRead Interceptor ========== */

func TestOnAfterReadInterceptor(t *testing.T) {
	mt := NewMockTransport()
	mt.On("GET", "https://api.test/path", MockResponse{StatusCode: 200, Body: []byte("OK")})

	var afterCalled bool
	c := New(
		HTTPClient(&http.Client{Transport: mt}),
		BaseURL("https://api.test"),
		OnAfterRead(func(resp *Response, raw *http.Response) {
			afterCalled = true
			if resp.StatusCode != 200 {
				t.Fatalf("unexpected status in after read")
			}
		}),
	)

	ctx := context.Background()
	c.Get(ctx, "/path")

	if !afterCalled {
		t.Fatalf("OnAfterRead not called")
	}
}
