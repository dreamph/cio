package cio

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Content types
const (
	// Application
	JSON          = "application/json"
	XML           = "application/xml"
	Form          = "application/x-www-form-urlencoded"
	MultipartForm = "multipart/form-data"
	OctetStream   = "application/octet-stream"
	PDF           = "application/pdf"
	ZIP           = "application/zip"
	GZIP          = "application/gzip"
	JS            = "application/javascript"
	WASM          = "application/wasm"
	GraphQL       = "application/graphql+json"
	YAML          = "application/x-yaml"
	MsgPack       = "application/msgpack"
	Protobuf      = "application/protobuf"
	CBOR          = "application/cbor"

	// Text
	Text        = "text/plain"
	HTML        = "text/html"
	CSS         = "text/css"
	CSV         = "text/csv"
	Markdown    = "text/markdown"
	EventStream = "text/event-stream"

	// Image
	PNG  = "image/png"
	JPEG = "image/jpeg"
	GIF  = "image/gif"
	WEBP = "image/webp"
	SVG  = "image/svg+xml"
	ICO  = "image/x-icon"
	AVIF = "image/avif"

	// Audio
	MP3  = "audio/mpeg"
	WAV  = "audio/wav"
	OGG  = "audio/ogg"
	FLAC = "audio/flac"
	AAC  = "audio/aac"

	// Video
	MP4  = "video/mp4"
	WEBM = "video/webm"
	AVI  = "video/x-msvideo"

	// Font
	WOFF  = "font/woff"
	WOFF2 = "font/woff2"
	TTF   = "font/ttf"
	OTF   = "font/otf"
)

// Buffer sizes
const (
	defaultBufferSize = 32 * 1024 // 32KB - optimal for most transfers
)

// Buffer pools for reducing allocations
var (
	bufferPool = sync.Pool{
		New: func() any {
			return bytes.NewBuffer(make([]byte, 0, defaultBufferSize))
		},
	}
	requestPool = sync.Pool{
		New: func() any {
			return &request{
				headers: make(map[string]string, 8),
				query:   make(map[string]string, 4),
			}
		},
	}
	copyBufPool = sync.Pool{
		New: func() any {
			return make([]byte, defaultBufferSize)
		},
	}
)

func getBuffer() *bytes.Buffer {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

func putBuffer(buf *bytes.Buffer) {
	// avoid pooling huge buffers if any code grows it unexpectedly
	if buf.Cap() <= 64*1024 {
		bufferPool.Put(buf)
	}
}

func getRequest() *request {
	r := requestPool.Get().(*request)
	r.body = nil
	r.bodyBytes = nil
	r.output = nil
	r.timeout = 0
	r.multipart = nil
	r.retry = 0
	r.retryBackoff = 0
	r.retryWhen = nil
	r.expectStatus = r.expectStatus[:0]
	for k := range r.headers {
		delete(r.headers, k)
	}
	for k := range r.query {
		delete(r.query, k)
	}
	return r
}

func putRequest(r *request) { requestPool.Put(r) }

func getCopyBuf() []byte { return copyBufPool.Get().([]byte) }

func putCopyBuf(b []byte) {
	// normalize back to default size to keep pool consistent
	if cap(b) >= defaultBufferSize {
		copyBufPool.Put(b[:defaultBufferSize])
		return
	}
	// if it's smaller for some reason, still put it back
	copyBufPool.Put(b)
}

// Errors
var (
	ErrUnexpectedStatus = errors.New("unexpected status code")
	ErrNoCookieJar      = errors.New("cookie jar not enabled, use WithCookieJar()")
)

// StatusError represents an HTTP status error
type StatusError struct {
	StatusCode int
	Status     string
	Body       []byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected status %d: %s", e.StatusCode, e.Status)
}

// Interceptors
type RequestInterceptor func(*http.Request)
type ResponseInterceptor func(*http.Response)

// Client
type Client struct {
	http       *http.Client
	baseURL    string
	onRequest  []RequestInterceptor
	onResponse []ResponseInterceptor
	requestID  func() string
}

type ClientOption func(*Client)

func BaseURL(u string) ClientOption {
	return func(c *Client) { c.baseURL = u }
}

func HTTPClient(hc *http.Client) ClientOption {
	return func(c *Client) { c.http = hc }
}

func OnRequest(fn RequestInterceptor) ClientOption {
	return func(c *Client) { c.onRequest = append(c.onRequest, fn) }
}

func OnResponse(fn ResponseInterceptor) ClientOption {
	return func(c *Client) { c.onResponse = append(c.onResponse, fn) }
}

// WithCookieJar enables cookie management
func WithCookieJar() ClientOption {
	return func(c *Client) {
		if c.http == nil {
			c.http = &http.Client{Transport: DefaultTransport()}
		}
		jar, _ := cookiejar.New(nil)
		c.http.Jar = jar
	}
}

// WithRedirects sets max redirects (0 = disable redirects)
func WithRedirects(max int) ClientOption {
	return func(c *Client) {
		if c.http == nil {
			c.http = &http.Client{Transport: DefaultTransport()}
		}
		c.http.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if max == 0 {
				return http.ErrUseLastResponse
			}
			if len(via) >= max {
				return fmt.Errorf("stopped after %d redirects", max)
			}
			return nil
		}
	}
}

// NoRedirects disables following redirects
func NoRedirects() ClientOption { return WithRedirects(0) }

// WithRequestID sets a function to generate request IDs (added as X-Request-ID header)
func WithRequestID(fn func() string) ClientOption {
	return func(c *Client) {
		c.requestID = fn
	}
}

// WithTracing adds common tracing headers generator
func WithTracing(serviceName string) ClientOption {
	return func(c *Client) {
		c.requestID = func() string {
			b := make([]byte, 16)
			if _, err := rand.Read(b); err != nil {
				// fallback: time-based (still unique enough for most use)
				return fmt.Sprintf("%d", time.Now().UnixNano())
			}
			return hex.EncodeToString(b)
		}
		c.onRequest = append(c.onRequest, func(req *http.Request) {
			req.Header.Set("X-Service-Name", serviceName)
		})
	}
}

// DefaultTransport returns an optimized http.Transport for high performance
func DefaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		MaxConnsPerHost:       100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
		ReadBufferSize:        defaultBufferSize,
		WriteBufferSize:       defaultBufferSize,
	}
}

func New(opts ...ClientOption) *Client {
	c := &Client{
		http: &http.Client{
			Transport: DefaultTransport(),
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.http == nil {
		c.http = &http.Client{Transport: DefaultTransport()}
	}
	return c
}

// SetCookies sets cookies for a URL
func (c *Client) SetCookies(rawURL string, cookies []*http.Cookie) error {
	if c.http == nil || c.http.Jar == nil {
		return ErrNoCookieJar
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	c.http.Jar.SetCookies(u, cookies)
	return nil
}

// Cookies returns cookies for a URL
func (c *Client) Cookies(rawURL string) ([]*http.Cookie, error) {
	if c.http == nil || c.http.Jar == nil {
		return nil, ErrNoCookieJar
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	return c.http.Jar.Cookies(u), nil
}

// Response
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Written    int64 // bytes written when using OutputStream
}

func (r *Response) OK() bool { return r.StatusCode >= 200 && r.StatusCode < 300 }

func (r *Response) String() string {
	if r.Body == nil {
		return ""
	}
	return string(r.Body)
}

func (r *Response) Json(v any) error { return json.Unmarshal(r.Body, v) }

// Json helper with generics
func Json[T any](r *Response) (T, error) {
	var v T
	err := json.Unmarshal(r.Body, &v)
	return v, err
}

// File represents a file for multipart upload
type File struct {
	Name   string    // form field name
	Path   string    // filename in the form
	Reader io.Reader // file content
	Size   int64     // optional: if known (not used for multipart Content-Length by default)
}

// NewFile creates a File from reader
func NewFile(fieldName, fileName string, r io.Reader) File {
	return File{Name: fieldName, Path: fileName, Reader: r}
}

// NewFileWithSize creates a File with known size
func NewFileWithSize(fieldName, fileName string, r io.Reader, size int64) File {
	return File{Name: fieldName, Path: fileName, Reader: r, Size: size}
}

// Multipart represents multipart form data
type Multipart struct {
	Files  []File
	Fields map[string]string
}

func (m Multipart) apply(r *request) { r.multipart = &m }

// R - Request config struct (also implements Option)
type R struct {
	Timeout time.Duration
	Headers map[string]string
	Query   map[string]string
	Body    any
}

func (cfg R) apply(r *request) {
	if cfg.Timeout > 0 {
		r.timeout = cfg.Timeout
	}
	for k, v := range cfg.Headers {
		r.headers[k] = v
	}
	for k, v := range cfg.Query {
		r.query[k] = v
	}
	if cfg.Body != nil {
		buf := getBuffer()
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(cfg.Body)
		b := buf.Bytes()
		if len(b) > 0 && b[len(b)-1] == '\n' {
			b = b[:len(b)-1]
		}
		r.bodyBytes = make([]byte, len(b))
		copy(r.bodyBytes, b)
		r.body = bytes.NewReader(r.bodyBytes)
		putBuffer(buf)
	}
}

// Options
type Option interface {
	apply(*request)
}

type optionFunc func(*request)

func (f optionFunc) apply(r *request) { f(r) }

type request struct {
	body         io.Reader
	bodyBytes    []byte
	output       io.Writer
	headers      map[string]string
	query        map[string]string
	timeout      time.Duration
	multipart    *Multipart
	retry        int
	retryBackoff time.Duration
	retryWhen    RetryCondition
	expectStatus []int
}

func Body(v any) Option {
	return optionFunc(func(r *request) {
		buf := getBuffer()
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(v)
		b := buf.Bytes()
		if len(b) > 0 && b[len(b)-1] == '\n' {
			b = b[:len(b)-1]
		}
		r.bodyBytes = make([]byte, len(b))
		copy(r.bodyBytes, b)
		r.body = bytes.NewReader(r.bodyBytes)
		putBuffer(buf)
	})
}

// BodyBytes sets pre-encoded body bytes (zero-copy for pre-marshaled data)
func BodyBytes(data []byte) Option {
	return optionFunc(func(r *request) {
		r.bodyBytes = data
		r.body = bytes.NewReader(data)
	})
}

func BodyReader(reader io.Reader) Option {
	return optionFunc(func(r *request) {
		r.body = reader
		r.bodyBytes = nil
	})
}

func OutputStream(w io.Writer) Option {
	return optionFunc(func(r *request) { r.output = w })
}

// Files creates a multipart upload with files only
func Files(files ...File) Option {
	return optionFunc(func(r *request) {
		if r.multipart == nil {
			r.multipart = &Multipart{}
		}
		r.multipart.Files = append(r.multipart.Files, files...)
	})
}

// FormFields adds form fields to multipart
func FormFields(fields map[string]string) Option {
	return optionFunc(func(r *request) {
		if r.multipart == nil {
			r.multipart = &Multipart{}
		}
		if r.multipart.Fields == nil {
			r.multipart.Fields = make(map[string]string, len(fields))
		}
		for k, v := range fields {
			r.multipart.Fields[k] = v
		}
	})
}

// RetryCondition determines if request should be retried
type RetryCondition func(resp *Response, err error) bool

// Retry sets retry count, backoff in ms, and optional condition
func Retry(count, backoffMs int, conditions ...RetryCondition) Option {
	return optionFunc(func(r *request) {
		r.retry = count
		r.retryBackoff = time.Duration(backoffMs) * time.Millisecond
		if len(conditions) > 0 {
			r.retryWhen = conditions[0]
		}
	})
}

// WhenStatus retries on specific status codes (O(1) lookup)
func WhenStatus(codes ...int) RetryCondition {
	codeMap := make(map[int]struct{}, len(codes))
	for _, c := range codes {
		codeMap[c] = struct{}{}
	}
	return func(resp *Response, err error) bool {
		if err != nil || resp == nil {
			return true
		}
		_, ok := codeMap[resp.StatusCode]
		return ok
	}
}

// When5xx retries on 5xx status codes
func When5xx() RetryCondition {
	return func(resp *Response, err error) bool {
		if err != nil || resp == nil {
			return true
		}
		return resp.StatusCode >= 500
	}
}

// WhenErr retries based on custom error check
func WhenErr(fn func(err error) bool) RetryCondition {
	return func(resp *Response, err error) bool {
		return err != nil && fn(err)
	}
}

// When retries based on custom condition
func When(fn func(resp *Response, err error) bool) RetryCondition { return fn }

// ExpectStatus sets expected status codes, returns error if not matched
func ExpectStatus(codes ...int) Option {
	return optionFunc(func(r *request) {
		r.expectStatus = append(r.expectStatus[:0], codes...)
	})
}

var okStatusCodes = []int{200, 201, 202, 203, 204, 205, 206, 207, 208, 226}

// ExpectOK expects 2xx status codes
func ExpectOK() Option {
	return optionFunc(func(r *request) {
		r.expectStatus = append(r.expectStatus[:0], okStatusCodes...)
	})
}

func Timeout(ms int) Option {
	return optionFunc(func(r *request) {
		r.timeout = time.Duration(ms) * time.Millisecond
	})
}

func Query(key, value string) Option {
	return optionFunc(func(r *request) {
		r.query[key] = value
	})
}

func QueryMap(params map[string]string) Option {
	return optionFunc(func(r *request) {
		for k, v := range params {
			r.query[k] = v
		}
	})
}

// Headers
type HeaderOption func(*request)

func Headers(opts ...HeaderOption) Option {
	return optionFunc(func(r *request) {
		for _, opt := range opts {
			opt(r)
		}
	})
}

func ContentType(ct string) HeaderOption { return func(r *request) { r.headers["Content-Type"] = ct } }
func Accept(ct string) HeaderOption      { return func(r *request) { r.headers["Accept"] = ct } }
func Header(key, value string) HeaderOption {
	return func(r *request) { r.headers[key] = value }
}

const bearerPrefix = "Bearer "

func Bearer(token string) HeaderOption {
	return func(r *request) { r.headers["Authorization"] = bearerPrefix + token }
}

// HTTP methods
func (c *Client) Get(ctx context.Context, path string, opts ...Option) (*Response, error) {
	return c.do(ctx, http.MethodGet, path, opts...)
}
func (c *Client) Post(ctx context.Context, path string, opts ...Option) (*Response, error) {
	return c.do(ctx, http.MethodPost, path, opts...)
}
func (c *Client) Put(ctx context.Context, path string, opts ...Option) (*Response, error) {
	return c.do(ctx, http.MethodPut, path, opts...)
}
func (c *Client) Patch(ctx context.Context, path string, opts ...Option) (*Response, error) {
	return c.do(ctx, http.MethodPatch, path, opts...)
}
func (c *Client) Delete(ctx context.Context, path string, opts ...Option) (*Response, error) {
	return c.do(ctx, http.MethodDelete, path, opts...)
}
func (c *Client) Head(ctx context.Context, path string, opts ...Option) (*Response, error) {
	return c.do(ctx, http.MethodHead, path, opts...)
}
func (c *Client) Options(ctx context.Context, path string, opts ...Option) (*Response, error) {
	return c.do(ctx, http.MethodOptions, path, opts...)
}

// Do executes a custom method request
func (c *Client) Do(ctx context.Context, method, path string, opts ...Option) (*Response, error) {
	return c.do(ctx, method, path, opts...)
}

// ParallelResult holds the result of a parallel request
type ParallelResult struct {
	Response *Response
	Error    error
	Index    int
}

// ParallelRequest represents a request for parallel execution
type ParallelRequest struct {
	Method string
	Path   string
	Opts   []Option
}

// Parallel executes multiple requests concurrently
func (c *Client) Parallel(ctx context.Context, requests []ParallelRequest) []ParallelResult {
	results := make([]ParallelResult, len(requests))
	var wg sync.WaitGroup
	wg.Add(len(requests))

	for i, req := range requests {
		go func(idx int, r ParallelRequest) {
			defer wg.Done()
			resp, err := c.do(ctx, r.Method, r.Path, r.Opts...)
			results[idx] = ParallelResult{Response: resp, Error: err, Index: idx}
		}(i, req)
	}

	wg.Wait()
	return results
}

// ParallelGet executes multiple GET requests concurrently
func (c *Client) ParallelGet(ctx context.Context, paths []string, opts ...Option) []ParallelResult {
	requests := make([]ParallelRequest, len(paths))
	for i, path := range paths {
		requests[i] = ParallelRequest{
			Method: http.MethodGet,
			Path:   path,
			Opts:   opts,
		}
	}
	return c.Parallel(ctx, requests)
}

func (c *Client) do(ctx context.Context, method, path string, opts ...Option) (*Response, error) {
	r := getRequest()
	defer putRequest(r)

	for _, opt := range opts {
		opt.apply(r)
	}

	var lastErr error
	var lastResp *Response
	maxAttempts := r.retry + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 && r.retryBackoff > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(r.retryBackoff * time.Duration(attempt)):
			}
		}

		resp, err := c.doOnce(ctx, method, path, r)
		lastResp = resp
		lastErr = err

		if err == nil && resp != nil && len(r.expectStatus) > 0 {
			if !containsInt(r.expectStatus, resp.StatusCode) {
				lastErr = &StatusError{
					StatusCode: resp.StatusCode,
					Status:     http.StatusText(resp.StatusCode),
					Body:       resp.Body,
				}
			}
		}

		if r.retryWhen != nil {
			if r.retryWhen(resp, lastErr) && attempt < maxAttempts-1 {
				continue
			}
		} else if lastErr != nil && attempt < maxAttempts-1 {
			continue
		}

		return resp, lastErr
	}

	return lastResp, lastErr
}

func (c *Client) doOnce(ctx context.Context, method, path string, r *request) (*Response, error) {
	if c.http == nil {
		c.http = &http.Client{Transport: DefaultTransport()}
	}

	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	fullURL, err := c.buildURL(path, r.query)
	if err != nil {
		return nil, err
	}

	var contentType string
	var body io.Reader = r.body

	if r.multipart != nil {
		body, contentType = buildMultipartStream(r.multipart)
	} else if r.bodyBytes != nil {
		body = bytes.NewReader(r.bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}

	// Set Content-Length for non-multipart known body (nice for some servers/proxies)
	if r.multipart == nil && r.bodyBytes != nil {
		req.ContentLength = int64(len(r.bodyBytes))
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	for k, v := range r.headers {
		req.Header.Set(k, v)
	}

	if c.requestID != nil {
		req.Header.Set("X-Request-ID", c.requestID())
	}

	for _, fn := range c.onRequest {
		fn(req)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	for _, fn := range c.onResponse {
		fn(resp)
	}

	result := &Response{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
	}

	if r.output != nil {
		result.Written, err = copyBuffered(r.output, resp.Body)
	} else {
		buf := getBuffer()
		_, err = buf.ReadFrom(resp.Body)
		if err == nil {
			result.Body = make([]byte, buf.Len())
			copy(result.Body, buf.Bytes())
		}
		putBuffer(buf)
	}

	if err != nil {
		return nil, err
	}

	return result, nil
}

// copyBuffered uses pooled buffer for efficient copying
func copyBuffered(dst io.Writer, src io.Reader) (int64, error) {
	buf := getCopyBuf()
	defer putCopyBuf(buf)
	return io.CopyBuffer(dst, src, buf)
}

func containsInt(slice []int, val int) bool {
	for _, v := range slice {
		if v == val {
			return true
		}
	}
	return false
}

// buildMultipartStream streams multipart content (no full buffering in RAM)
func buildMultipartStream(m *Multipart) (io.Reader, string) {
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)

	go func() {
		defer func() {
			_ = w.Close()
			_ = pw.Close()
		}()

		// files
		for _, f := range m.Files {
			filename := f.Path
			if filename == "" {
				// use field name as fallback (keeps same behavior as original code)
				filename = filepath.Base(f.Name)
			}
			part, err := w.CreateFormFile(f.Name, filename)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if _, err := copyBuffered(part, f.Reader); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}

		// fields
		for k, v := range m.Fields {
			if err := w.WriteField(k, v); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()

	return pr, w.FormDataContentType()
}

func (c *Client) buildURL(path string, query map[string]string) (string, error) {
	// Fast path: no query changes
	if len(query) == 0 && c.baseURL == "" {
		return path, nil
	}

	var u *url.URL
	if c.baseURL != "" {
		base, err := url.Parse(c.baseURL)
		if err != nil {
			return "", err
		}
		ref, err := url.Parse(path)
		if err != nil {
			return "", err
		}
		u = base.ResolveReference(ref)
	} else {
		parsed, err := url.Parse(path)
		if err != nil {
			return "", err
		}
		u = parsed
	}

	// Merge query safely (handles existing ?a=1 already in path)
	if len(query) > 0 {
		q := u.Query()
		for k, v := range query {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}

	// Keep behavior for relative paths without baseURL (but normalize)
	s := u.String()

	// url.Parse("abc") becomes Path "abc" and String "abc" ok;
	// just ensure we don't introduce leading scheme-less weirdness
	if c.baseURL == "" && !strings.Contains(path, "://") {
		// if input was raw path like "/x", u.String() keeps it
		return s, nil
	}
	return s, nil
}
