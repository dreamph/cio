package cio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// Test Helpers
// =============================================================================

func newTestServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

func jsonHandler(data any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	}
}

func echoHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
		w.Header().Set("X-Method", r.Method)
		w.Header().Set("X-Path", r.URL.Path)
		w.Header().Set("X-Query", r.URL.RawQuery)
		for k, v := range r.Header {
			w.Header().Set("X-Req-"+k, strings.Join(v, ","))
		}
		w.Write(body)
	}
}

// =============================================================================
// Basic Client Tests
// =============================================================================

func TestNew(t *testing.T) {
	c := New()
	if c == nil {
		t.Fatal("New() returned nil")
	}
	if c.http == nil {
		t.Fatal("http.Client is nil")
	}
}

func TestNewWithOptions(t *testing.T) {
	c := New(
		BaseURL("http://example.com"),
		WithUserAgent("test-agent"),
	)
	if c.baseURL != "http://example.com" {
		t.Errorf("expected baseURL http://example.com, got %s", c.baseURL)
	}
	if c.userAgent != "test-agent" {
		t.Errorf("expected userAgent test-agent, got %s", c.userAgent)
	}
}

func TestDefaultTransport(t *testing.T) {
	tr := DefaultTransport()
	if tr.MaxIdleConns != 100 {
		t.Errorf("expected MaxIdleConns 100, got %d", tr.MaxIdleConns)
	}
}

// =============================================================================
// HTTP Method Tests
// =============================================================================

func TestGet(t *testing.T) {
	ts := newTestServer(jsonHandler(map[string]string{"status": "ok"}))
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Get(context.Background(), "/test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if !resp.OK() {
		t.Error("expected OK() to be true")
	}
}

func TestPost(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Post(context.Background(), "/test",
		JSONBody(map[string]string{"name": "test"}),
	)
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	var result map[string]string
	if err := resp.Json(&result); err != nil {
		t.Fatalf("Json decode failed: %v", err)
	}
	if result["name"] != "test" {
		t.Errorf("expected name=test, got %s", result["name"])
	}
}

func TestPut(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Put(context.Background(), "/test", JSONBody(map[string]int{"id": 1}))
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if resp.Headers.Get("X-Method") != "PUT" {
		t.Errorf("expected method PUT")
	}
}

func TestPatch(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Patch(context.Background(), "/test", JSONBody(map[string]int{"id": 1}))
	if err != nil {
		t.Fatalf("Patch failed: %v", err)
	}
	if resp.Headers.Get("X-Method") != "PATCH" {
		t.Errorf("expected method PATCH")
	}
}

func TestDelete(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Delete(context.Background(), "/test")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if resp.Headers.Get("X-Method") != "DELETE" {
		t.Errorf("expected method DELETE")
	}
}

func TestHead(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "value")
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Head(context.Background(), "/test")
	if err != nil {
		t.Fatalf("Head failed: %v", err)
	}
	if resp.Headers.Get("X-Custom") != "value" {
		t.Error("expected X-Custom header")
	}
}

func TestOptions(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "GET, POST")
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Options(context.Background(), "/test")
	if err != nil {
		t.Fatalf("Options failed: %v", err)
	}
	if resp.Headers.Get("Allow") == "" {
		t.Error("expected Allow header")
	}
}

func TestDo(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Do(context.Background(), "CUSTOM", "/test")
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	if resp.Headers.Get("X-Method") != "CUSTOM" {
		t.Errorf("expected method CUSTOM")
	}
}

// =============================================================================
// Query Parameters Tests
// =============================================================================

func TestQuerySet(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Get(context.Background(), "/test",
		QuerySet("page", "1"),
		QuerySet("limit", "10"),
	)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	query := resp.Headers.Get("X-Query")
	if !strings.Contains(query, "page=1") {
		t.Errorf("expected page=1 in query, got %s", query)
	}
}

func TestQueryAdd(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Get(context.Background(), "/test",
		QueryAdd("tag", "a"),
		QueryAdd("tag", "b"),
	)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	query := resp.Headers.Get("X-Query")
	if !strings.Contains(query, "tag=a") || !strings.Contains(query, "tag=b") {
		t.Errorf("expected multi-value tag, got %s", query)
	}
}

func TestQueryValues(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	values := url.Values{}
	values.Add("foo", "bar")

	resp, err := c.Get(context.Background(), "/test", QueryValues(values))
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	query := resp.Headers.Get("X-Query")
	if !strings.Contains(query, "foo=bar") {
		t.Errorf("expected foo=bar, got %s", query)
	}
}

// =============================================================================
// Headers Tests
// =============================================================================

func TestHeaders(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Get(context.Background(), "/test",
		Headers(
			Header("X-Custom", "value"),
			ContentType(JSON),
			Accept(JSON),
		),
	)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if resp.Headers.Get("X-Req-X-Custom") != "value" {
		t.Error("expected X-Custom header")
	}
}

func TestBearer(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Get(context.Background(), "/test", Headers(Bearer("my-token")))
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	auth := resp.Headers.Get("X-Req-Authorization")
	if auth != "Bearer my-token" {
		t.Errorf("expected Bearer my-token, got %s", auth)
	}
}

func TestWithDefaultHeaders(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(
		BaseURL(ts.URL),
		WithDefaultHeaders(map[string]string{"X-Api-Key": "secret"}),
	)
	resp, err := c.Get(context.Background(), "/test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if resp.Headers.Get("X-Req-X-Api-Key") != "secret" {
		t.Error("expected X-Api-Key header")
	}
}

func TestWithUserAgent(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL), WithUserAgent("my-client/1.0"))
	resp, err := c.Get(context.Background(), "/test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if resp.Headers.Get("X-Req-User-Agent") != "my-client/1.0" {
		t.Error("expected User-Agent header")
	}
}

func TestWithBasicAuth(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || pass != "secret" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL), WithBasicAuth("admin", "secret"))
	resp, err := c.Get(context.Background(), "/test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// =============================================================================
// Body Tests
// =============================================================================

func TestJSONBody(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Post(context.Background(), "/test",
		JSONBody(map[string]string{"key": "value"}),
	)
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	var result map[string]string
	if err := resp.Json(&result); err != nil {
		t.Fatalf("Json decode failed: %v", err)
	}
	if result["key"] != "value" {
		t.Errorf("expected key=value")
	}
}

func TestBodyBytes(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	data := []byte(`{"raw":"data"}`)
	resp, err := c.Post(context.Background(), "/test",
		BodyBytes(data),
		Headers(ContentType(JSON)),
	)
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if resp.String() != string(data) {
		t.Errorf("expected %s, got %s", string(data), resp.String())
	}
}

func TestBodyReader(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	reader := bytes.NewReader([]byte("hello world"))
	resp, err := c.Post(context.Background(), "/test",
		BodyReader(reader),
		Headers(ContentType(Text)),
	)
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if resp.String() != "hello world" {
		t.Errorf("expected 'hello world', got %s", resp.String())
	}
}

func TestBodyFunc(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Post(context.Background(), "/test",
		BodyFunc(func() (io.ReadCloser, string, int64, error) {
			return io.NopCloser(strings.NewReader("factory body")), Text, 12, nil
		}),
	)
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if resp.String() != "factory body" {
		t.Errorf("expected 'factory body', got %s", resp.String())
	}
}

// =============================================================================
// Response Tests
// =============================================================================

func TestResponseOK(t *testing.T) {
	tests := []struct {
		status int
		ok     bool
	}{
		{200, true}, {201, true}, {299, true},
		{300, false}, {400, false}, {500, false},
	}

	for _, tt := range tests {
		r := &Response{StatusCode: tt.status}
		if r.OK() != tt.ok {
			t.Errorf("Response{%d}.OK() = %v, want %v", tt.status, r.OK(), tt.ok)
		}
	}
}

func TestResponseString(t *testing.T) {
	r := &Response{Body: []byte("hello")}
	if r.String() != "hello" {
		t.Errorf("expected 'hello'")
	}

	r2 := &Response{Body: nil}
	if r2.String() != "" {
		t.Error("expected empty string for nil body")
	}
}

func TestJsonGeneric(t *testing.T) {
	r := &Response{Body: []byte(`{"id":123}`)}

	type Result struct {
		ID int `json:"id"`
	}

	result, err := Json[Result](r)
	if err != nil {
		t.Fatalf("Json failed: %v", err)
	}
	if result.ID != 123 {
		t.Errorf("expected id=123")
	}
}

// =============================================================================
// Timeout Tests
// =============================================================================

func TestTimeout(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(200)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	_, err := c.Get(context.Background(), "/test", Timeout(50))
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

// =============================================================================
// Retry Tests
// =============================================================================

func TestRetryOnError(t *testing.T) {
	attempts := int32(0)
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Get(context.Background(), "/test",
		Retry(3, 10, 100, When5xx()),
	)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts)
	}
}

func TestRetryWhenStatus(t *testing.T) {
	attempts := int32(0)
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Get(context.Background(), "/test",
		Retry(3, 10, 100, WhenStatus(429, 500)),
	)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200")
	}
}

func TestRetryNonRepeatableBody(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	// Use a pipe reader which is definitely not seekable
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte("data"))
		pw.Close()
	}()
	_, err := c.Post(context.Background(), "/test",
		BodyReader(pr),
		Retry(3, 10, 100),
	)
	if !errors.Is(err, ErrNonRepeatable) {
		t.Errorf("expected ErrNonRepeatable, got %v", err)
	}
}

func TestRetryWithBodyFunc(t *testing.T) {
	attempts := int32(0)
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 2 {
			w.WriteHeader(500)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Post(context.Background(), "/test",
		BodyFunc(func() (io.ReadCloser, string, int64, error) {
			return io.NopCloser(strings.NewReader("retry body")), Text, 10, nil
		}),
		Retry(3, 10, 100, When5xx()),
	)
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}
	if resp.String() != "retry body" {
		t.Errorf("expected 'retry body'")
	}
}

// =============================================================================
// ExpectStatus Tests
// =============================================================================

func TestExpectStatus(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Post(context.Background(), "/test", ExpectStatus(200, 201))
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("expected 201")
	}
}

func TestExpectStatusError(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte("bad request"))
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Get(context.Background(), "/test", ExpectStatus(200))
	if err == nil {
		t.Fatal("expected error")
	}

	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected StatusError, got %T", err)
	}
	if statusErr.StatusCode != 400 {
		t.Errorf("expected 400")
	}
	if resp == nil {
		t.Fatal("expected response even on error")
	}
}

func TestExpectOK(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Delete(context.Background(), "/test", ExpectOK())
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if resp.StatusCode != 204 {
		t.Errorf("expected 204")
	}
}

// =============================================================================
// Streaming Tests
// =============================================================================

func TestOutputStream(t *testing.T) {
	data := strings.Repeat("x", 10000)
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(data))
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	var buf bytes.Buffer
	resp, err := c.Get(context.Background(), "/test", OutputStream(&buf))
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if resp.Written != int64(len(data)) {
		t.Errorf("expected Written=%d, got %d", len(data), resp.Written)
	}
	if buf.String() != data {
		t.Error("buffer content mismatch")
	}
}

func TestOutputStreamToFile(t *testing.T) {
	data := "file content"
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(data))
	})
	defer ts.Close()

	tmpFile, err := os.CreateTemp("", "cio-test-*")
	if err != nil {
		t.Fatalf("CreateTemp failed: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	c := New(BaseURL(ts.URL))
	resp, err := c.Get(context.Background(), "/test", OutputStream(tmpFile))
	tmpFile.Close()
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if resp.Written != int64(len(data)) {
		t.Errorf("expected Written=%d", len(data))
	}

	content, _ := os.ReadFile(tmpFile.Name())
	if string(content) != data {
		t.Error("file content mismatch")
	}
}

// =============================================================================
// MaxBodyBytes Tests
// =============================================================================

func TestMaxBodyBytes(t *testing.T) {
	data := strings.Repeat("x", 1000)
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(data))
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	_, err := c.Get(context.Background(), "/test", MaxBodyBytes(100))
	if err == nil {
		t.Fatal("expected error")
	}

	var bodyErr *BodyTooLargeError
	if !errors.As(err, &bodyErr) {
		t.Fatalf("expected BodyTooLargeError, got %T: %v", err, err)
	}
}

// =============================================================================
// Multipart Tests
// =============================================================================

func TestMultipartFiles(t *testing.T) {
	var receivedFile []byte
	var receivedFilename string
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(32 << 20)
		file, header, err := r.FormFile("upload")
		if err != nil {
			w.WriteHeader(400)
			return
		}
		defer file.Close()
		receivedFile, _ = io.ReadAll(file)
		receivedFilename = header.Filename
		w.WriteHeader(200)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Post(context.Background(), "/upload",
		Files(NewFile("upload", "test.txt", strings.NewReader("file content"))),
	)
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200")
	}
	if string(receivedFile) != "file content" {
		t.Error("file content mismatch")
	}
	if receivedFilename != "test.txt" {
		t.Errorf("expected filename test.txt")
	}
}

func TestMultipartWithFields(t *testing.T) {
	var receivedField string
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(32 << 20)
		receivedField = r.FormValue("title")
		w.WriteHeader(200)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Post(context.Background(), "/upload",
		Files(NewFile("file", "doc.txt", strings.NewReader("content"))),
		FormFields(map[string]string{"title": "My Document"}),
	)
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200")
	}
	if receivedField != "My Document" {
		t.Errorf("expected title 'My Document'")
	}
}

func TestMultipartStruct(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(32 << 20)
		if r.FormValue("field1") != "value1" {
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(200)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Post(context.Background(), "/upload", Multipart{
		Files:  []File{NewFile("file", "test.txt", strings.NewReader("data"))},
		Fields: map[string]string{"field1": "value1"},
	})
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200")
	}
}

func TestMultipartWithOpen(t *testing.T) {
	attempts := int32(0)
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		r.ParseMultipartForm(32 << 20)
		file, _, _ := r.FormFile("file")
		content, _ := io.ReadAll(file)
		file.Close()
		if n < 2 {
			w.WriteHeader(500)
			return
		}
		w.Write(content)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Post(context.Background(), "/upload",
		Files(File{
			Name: "file",
			Path: "data.txt",
			Open: func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("retry content")), nil
			},
		}),
		Retry(3, 10, 100, When5xx()),
	)
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}
	if resp.String() != "retry content" {
		t.Errorf("expected 'retry content'")
	}
}

// =============================================================================
// Cookie Tests
// =============================================================================

func TestWithCookieJar(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc123"})
			return
		}
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value != "abc123" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL), WithCookieJar())
	c.Get(context.Background(), "/login")

	resp, err := c.Get(context.Background(), "/protected")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200")
	}
}

func TestSetCookies(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("manual")
		if err != nil || cookie.Value != "cookie" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL), WithCookieJar())
	c.SetCookies(ts.URL, []*http.Cookie{{Name: "manual", Value: "cookie"}})

	resp, err := c.Get(context.Background(), "/test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200")
	}
}

func TestNoCookieJarError(t *testing.T) {
	c := New()
	err := c.SetCookies("http://example.com", nil)
	if !errors.Is(err, ErrNoCookieJar) {
		t.Errorf("expected ErrNoCookieJar")
	}
}

// =============================================================================
// Redirect Tests
// =============================================================================

func TestWithRedirects(t *testing.T) {
	redirectCount := 0
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if redirectCount < 3 {
			redirectCount++
			http.Redirect(w, r, "/redirect", http.StatusFound)
			return
		}
		w.WriteHeader(200)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL), WithRedirects(5))
	resp, err := c.Get(context.Background(), "/start")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("expected 200")
	}
}

func TestNoRedirects(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", http.StatusFound)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL), NoRedirects())
	resp, err := c.Get(context.Background(), "/test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if resp.StatusCode != 302 {
		t.Errorf("expected 302")
	}
}

// =============================================================================
// Interceptor Tests
// =============================================================================

func TestOnRequest(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	called := false
	c := New(
		BaseURL(ts.URL),
		OnRequest(func(req *http.Request) {
			called = true
			req.Header.Set("X-Intercepted", "true")
		}),
	)

	resp, err := c.Get(context.Background(), "/test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !called {
		t.Error("OnRequest not called")
	}
	if resp.Headers.Get("X-Req-X-Intercepted") != "true" {
		t.Error("expected intercepted header")
	}
}

func TestOnResponse(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Server", "test")
		w.WriteHeader(200)
	})
	defer ts.Close()

	var capturedHeader string
	c := New(
		BaseURL(ts.URL),
		OnResponse(func(resp *http.Response) {
			capturedHeader = resp.Header.Get("X-Server")
		}),
	)

	c.Get(context.Background(), "/test")
	if capturedHeader != "test" {
		t.Error("expected X-Server in interceptor")
	}
}

func TestOnAfterRead(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("response body"))
	})
	defer ts.Close()

	var capturedBody string
	c := New(
		BaseURL(ts.URL),
		OnAfterRead(func(resp *Response, raw *http.Response) {
			capturedBody = resp.String()
		}),
	)

	c.Get(context.Background(), "/test")
	if capturedBody != "response body" {
		t.Error("expected body in OnAfterRead")
	}
}

// =============================================================================
// RequestID/Tracing Tests
// =============================================================================

func TestWithRequestID(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(
		BaseURL(ts.URL),
		WithRequestID(func() string { return "test-id-123" }),
	)

	resp, err := c.Get(context.Background(), "/test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if resp.Headers.Get("X-Req-X-Request-Id") != "test-id-123" {
		t.Error("expected X-Request-ID")
	}
}

func TestWithTracing(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL), WithTracing("my-service"))

	resp, err := c.Get(context.Background(), "/test")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if resp.Headers.Get("X-Req-X-Service-Name") != "my-service" {
		t.Error("expected X-Service-Name")
	}
	if resp.Headers.Get("X-Req-X-Request-Id") == "" {
		t.Error("expected X-Request-ID")
	}
}

// =============================================================================
// Parallel Tests
// =============================================================================

func TestParallel(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.URL.Path))
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	results := c.Parallel(context.Background(), []ParallelRequest{
		{Method: "GET", Path: "/a"},
		{Method: "GET", Path: "/b"},
		{Method: "POST", Path: "/c", Opts: []Option{JSONBody(map[string]int{"id": 1})}},
	})

	if len(results) != 3 {
		t.Fatalf("expected 3 results")
	}

	for _, r := range results {
		if r.Error != nil {
			t.Errorf("request %d failed: %v", r.Index, r.Error)
		}
	}
}

func TestParallelGet(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.URL.Path))
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	results := c.ParallelGet(context.Background(), []string{"/1", "/2", "/3"})

	if len(results) != 3 {
		t.Fatalf("expected 3 results")
	}

	for i, r := range results {
		if r.Error != nil {
			t.Errorf("request %d failed: %v", i, r.Error)
		}
		expected := "/" + strconv.Itoa(i+1)
		if r.Response.String() != expected {
			t.Errorf("expected %s, got %s", expected, r.Response.String())
		}
	}
}

// =============================================================================
// R Struct Tests
// =============================================================================

func TestRStruct(t *testing.T) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	resp, err := c.Post(context.Background(), "/test", R{
		Timeout: 5 * time.Second,
		Headers: map[string]string{"X-Custom": "value"},
		Query:   map[string]string{"page": "1"},
		Body:    map[string]string{"name": "test"},
	})
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if resp.Headers.Get("X-Req-X-Custom") != "value" {
		t.Error("expected X-Custom")
	}
	if !strings.Contains(resp.Headers.Get("X-Query"), "page=1") {
		t.Error("expected page=1 in query")
	}
}

// =============================================================================
// Pool Tests
// =============================================================================

func TestBufferPool(t *testing.T) {
	buf := getBuffer()
	if buf == nil {
		t.Fatal("getBuffer returned nil")
	}
	buf.WriteString("test")
	putBuffer(buf)

	buf2 := getBuffer()
	if buf2.Len() != 0 {
		t.Error("buffer should be reset")
	}
	putBuffer(buf2)
}

func TestRequestPool(t *testing.T) {
	r := getRequest()
	if r == nil {
		t.Fatal("getRequest returned nil")
	}
	r.timeout = 5 * time.Second
	r.headers["key"] = "value"
	putRequest(r)

	r2 := getRequest()
	if r2.timeout != 0 {
		t.Error("request timeout should be reset")
	}
	if len(r2.headers) != 0 {
		t.Error("request headers should be cleared")
	}
	putRequest(r2)
}

// =============================================================================
// Content Types Test
// =============================================================================

func TestContentTypes(t *testing.T) {
	if JSON != "application/json" {
		t.Error("JSON constant incorrect")
	}
	if Text != "text/plain" {
		t.Error("Text constant incorrect")
	}
	if PNG != "image/png" {
		t.Error("PNG constant incorrect")
	}
}

// =============================================================================
// Error Types Test
// =============================================================================

func TestStatusErrorMessage(t *testing.T) {
	err := &StatusError{StatusCode: 404, Status: "Not Found"}
	msg := err.Error()
	if !strings.Contains(msg, "404") {
		t.Errorf("error should contain 404: %s", msg)
	}
}

func TestBodyTooLargeErrorMessage(t *testing.T) {
	err := &BodyTooLargeError{Limit: 1000}
	msg := err.Error()
	if !strings.Contains(msg, "1000") {
		t.Errorf("error should contain limit: %s", msg)
	}
}

// =============================================================================
// Concurrent Safety Test
// =============================================================================

func TestConcurrentRequests(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.Write([]byte("ok"))
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))

	var wg sync.WaitGroup
	errs := make(chan error, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := c.Get(context.Background(), "/test")
			if err != nil {
				errs <- err
				return
			}
			if resp.StatusCode != 200 {
				errs <- fmt.Errorf("unexpected status: %d", resp.StatusCode)
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent request failed: %v", err)
	}
}

// =============================================================================
// Context Cancellation Test
// =============================================================================

func TestContextCancellation(t *testing.T) {
	ts := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(200)
	})
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := c.Get(ctx, "/test")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// =============================================================================
// Benchmark Tests
// =============================================================================

func BenchmarkGet(b *testing.B) {
	ts := newTestServer(jsonHandler(map[string]string{"status": "ok"}))
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Get(ctx, "/test")
	}
}

func BenchmarkPost(b *testing.B) {
	ts := newTestServer(echoHandler())
	defer ts.Close()

	c := New(BaseURL(ts.URL))
	ctx := context.Background()
	body := map[string]string{"name": "test"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Post(ctx, "/test", JSONBody(body))
	}
}

func BenchmarkBufferPool(b *testing.B) {
	for i := 0; i < b.N; i++ {
		buf := getBuffer()
		buf.WriteString("test data")
		putBuffer(buf)
	}
}

func BenchmarkRequestPool(b *testing.B) {
	for i := 0; i < b.N; i++ {
		r := getRequest()
		r.timeout = time.Second
		r.headers["key"] = "value"
		putRequest(r)
	}
}
