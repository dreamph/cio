package cio

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	mrand "math/rand"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"sync"
	"time"
)

// Content types
const (
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
	Text          = "text/plain"
	HTML          = "text/html"
	CSS           = "text/css"
	CSV           = "text/csv"
	Markdown      = "text/markdown"
	EventStream   = "text/event-stream"
	PNG           = "image/png"
	JPEG          = "image/jpeg"
	GIF           = "image/gif"
	WEBP          = "image/webp"
	SVG           = "image/svg+xml"
	ICO           = "image/x-icon"
	AVIF          = "image/avif"
	MP3           = "audio/mpeg"
	WAV           = "audio/wav"
	OGG           = "audio/ogg"
	FLAC          = "audio/flac"
	AAC           = "audio/aac"
	MP4           = "video/mp4"
	WEBM          = "video/webm"
	AVI           = "video/x-msvideo"
	WOFF          = "font/woff"
	WOFF2         = "font/woff2"
	TTF           = "font/ttf"
	OTF           = "font/otf"
)

// Buffer sizes
const (
	defaultBufferSize = 32 * 1024
	maxPoolBufferCap  = 64 * 1024
)

// Pools
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
				query:   make(url.Values, 4),
			}
		},
	}
	copyBufPool = sync.Pool{
		New: func() any { return make([]byte, defaultBufferSize) },
	}
)

// rng for jitter
var (
	rngMu sync.Mutex
	rng   = mrand.New(mrand.NewSource(time.Now().UnixNano()))
)

func jitter01() float64 {
	rngMu.Lock()
	v := rng.Float64()
	rngMu.Unlock()
	return v
}

func getBuffer() *bytes.Buffer {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}
func putBuffer(buf *bytes.Buffer) {
	if buf.Cap() <= maxPoolBufferCap {
		bufferPool.Put(buf)
	}
}

func getCopyBuf() []byte { return copyBufPool.Get().([]byte) }
func putCopyBuf(b []byte) {
	if cap(b) >= defaultBufferSize {
		copyBufPool.Put(b[:defaultBufferSize])
		return
	}
	copyBufPool.Put(b)
}

func getRequest() *request {
	r := requestPool.Get().(*request)

	r.timeout = 0
	r.output = nil

	r.bodyBytes = nil
	r.bodyFactory = nil
	r.bodyContentType = ""
	r.bodyContentLength = -1
	r.nonRepeatableBody = false

	r.multipart = nil

	r.retry = 0
	r.retryBase = 0
	r.retryMax = 0
	r.retryWhen = nil

	r.maxBodyBytes = 0

	r.expectStatusSet = nil
	r.expectStatusList = r.expectStatusList[:0]

	for k := range r.headers {
		delete(r.headers, k)
	}
	// clear url.Values
	for k := range r.query {
		delete(r.query, k)
	}

	return r
}
func putRequest(r *request) { requestPool.Put(r) }

// Errors
var (
	ErrUnexpectedStatus = errors.New("unexpected status code")
	ErrNoCookieJar      = errors.New("cookie jar not enabled, use WithCookieJar()")
	ErrBodyTooLarge     = errors.New("response body exceeds MaxBodyBytes")
	ErrNonRepeatable    = errors.New("request body is non-repeatable; retry is not supported without BodyFunc/seekable body")
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

type BodyTooLargeError struct {
	Limit int64
}

func (e *BodyTooLargeError) Error() string {
	return fmt.Sprintf("%v: limit=%d", ErrBodyTooLarge, e.Limit)
}

// Interceptors
type RequestInterceptor func(*http.Request)
type ResponseInterceptor func(*http.Response)
type AfterReadInterceptor func(resp *Response, raw *http.Response)

// Client
type Client struct {
	http    *http.Client
	baseURL string

	onRequest  []RequestInterceptor
	onResponse []ResponseInterceptor
	onAfter    []AfterReadInterceptor

	requestID func() string

	// defaults
	defaultHeaders map[string]string
	userAgent      string
	basicUser      string
	basicPass      string
}

type ClientOption func(*Client)

func BaseURL(u string) ClientOption {
	return func(c *Client) { c.baseURL = u }
}

func HTTPClient(hc *http.Client) ClientOption {
	return func(c *Client) { c.http = hc }
}

func Transport(t *http.Transport) ClientOption {
	return func(c *Client) {
		if c.http == nil {
			c.http = &http.Client{}
		}
		c.http.Transport = t
	}
}

func OnRequest(fn RequestInterceptor) ClientOption {
	return func(c *Client) { c.onRequest = append(c.onRequest, fn) }
}
func OnResponse(fn ResponseInterceptor) ClientOption {
	return func(c *Client) { c.onResponse = append(c.onResponse, fn) }
}
func OnAfterRead(fn AfterReadInterceptor) ClientOption {
	return func(c *Client) { c.onAfter = append(c.onAfter, fn) }
}

func WithDefaultHeaders(h map[string]string) ClientOption {
	return func(c *Client) {
		if c.defaultHeaders == nil {
			c.defaultHeaders = make(map[string]string, len(h))
		}
		for k, v := range h {
			c.defaultHeaders[k] = v
		}
	}
}

func WithUserAgent(ua string) ClientOption {
	return func(c *Client) { c.userAgent = ua }
}

func WithBasicAuth(user, pass string) ClientOption {
	return func(c *Client) {
		c.basicUser = user
		c.basicPass = pass
	}
}

// WithCookieJar enables cookie management
func WithCookieJar() ClientOption {
	return func(c *Client) {
		c.ensureHTTP()
		jar, _ := cookiejar.New(nil)
		c.http.Jar = jar
	}
}

// WithRedirects sets max redirects (0 = disable redirects)
func WithRedirects(max int) ClientOption {
	return func(c *Client) {
		c.ensureHTTP()
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

func NoRedirects() ClientOption { return WithRedirects(0) }

// WithRequestID sets a function to generate request IDs (added as X-Request-ID header)
func WithRequestID(fn func() string) ClientOption {
	return func(c *Client) { c.requestID = fn }
}

// WithTracing adds common tracing headers generator
func WithTracing(serviceName string) ClientOption {
	return func(c *Client) {
		c.requestID = func() string {
			b := make([]byte, 16)
			if _, err := crand.Read(b); err != nil {
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
		http: &http.Client{Transport: DefaultTransport()},
	}
	for _, opt := range opts {
		opt(c)
	}
	c.ensureHTTP()
	return c
}

func (c *Client) ensureHTTP() {
	if c.http == nil {
		c.http = &http.Client{Transport: DefaultTransport()}
	}
	if c.http.Transport == nil {
		c.http.Transport = DefaultTransport()
	}
}

// SetCookies sets cookies for a URL
func (c *Client) SetCookies(rawURL string, cookies []*http.Cookie) error {
	c.ensureHTTP()
	if c.http.Jar == nil {
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
	c.ensureHTTP()
	if c.http.Jar == nil {
		return nil, ErrNoCookieJar
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	return c.http.Jar.Cookies(u), nil
}

// CloseIdleConnections closes idle connections held by the underlying transport
func (c *Client) CloseIdleConnections() {
	if tr, ok := c.http.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
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

func Json[T any](r *Response) (T, error) {
	var v T
	err := json.Unmarshal(r.Body, &v)
	return v, err
}

// File represents a file for multipart upload
// Retry notes:
// - If Open is provided, it will be called per-attempt (supports retry).
// - Else if Reader is io.ReadSeeker, it will be Seek(0,0) per-attempt.
// - Else multipart is non-repeatable => retry not allowed.
type File struct {
	Name string
	Path string

	Reader io.Reader
	Open   func() (io.ReadCloser, error) // optional factory for retry-safe

	Size int64 // optional; not used to compute multipart Content-Length by default
}

func NewFile(fieldName, fileName string, r io.Reader) File {
	return File{Name: fieldName, Path: fileName, Reader: r}
}
func NewFileWithSize(fieldName, fileName string, r io.Reader, size int64) File {
	return File{Name: fieldName, Path: fileName, Reader: r, Size: size}
}

// Multipart represents multipart form data
type Multipart struct {
	Files  []File
	Fields map[string]string
}

func (m Multipart) apply(r *request) { r.multipart = &m }

// Request config (also implements Option)
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
		r.query.Set(k, v)
	}
	if cfg.Body != nil {
		JSONBody(cfg.Body).apply(r)
	}
}

// Options
type Option interface{ apply(*request) }
type optionFunc func(*request)

func (f optionFunc) apply(r *request) { f(r) }

// Internal request
type request struct {
	timeout time.Duration
	output  io.Writer

	// headers/query
	headers map[string]string
	query   url.Values

	// body
	bodyBytes         []byte
	bodyFactory       func() (io.ReadCloser, string, int64, error) // reader, content-type, content-length
	bodyContentType   string
	bodyContentLength int64
	nonRepeatableBody bool

	// multipart (built via bodyFactory per attempt)
	multipart *Multipart

	// retry
	retry     int
	retryBase time.Duration
	retryMax  time.Duration
	retryWhen RetryCondition

	// expectations
	expectStatusList []int
	expectStatusSet  map[int]struct{}

	// response guard
	maxBodyBytes int64
}

// BodyFunc provides a repeatable body factory (retry-safe).
func BodyFunc(fn func() (io.ReadCloser, string, int64, error)) Option {
	return optionFunc(func(r *request) {
		r.bodyFactory = fn
		r.nonRepeatableBody = false
	})
}

// BodyBytes sets pre-encoded body bytes (retry-safe).
func BodyBytes(data []byte) Option {
	return optionFunc(func(r *request) {
		r.bodyBytes = data
		r.bodyFactory = func() (io.ReadCloser, string, int64, error) {
			return io.NopCloser(bytes.NewReader(data)), r.bodyContentType, int64(len(data)), nil
		}
		r.nonRepeatableBody = false
	})
}

// BodyReader sets a reader. Retry works only if reader is io.ReadSeeker.
func BodyReader(reader io.Reader) Option {
	return optionFunc(func(r *request) {
		// clear bytes
		r.bodyBytes = nil

		if rs, ok := reader.(io.ReadSeeker); ok {
			r.bodyFactory = func() (io.ReadCloser, string, int64, error) {
				if _, err := rs.Seek(0, io.SeekStart); err != nil {
					return nil, "", -1, err
				}
				// ReadSeeker isn't necessarily ReadCloser
				if rc, ok := reader.(io.ReadCloser); ok {
					return rc, r.bodyContentType, r.bodyContentLength, nil
				}
				return io.NopCloser(reader), r.bodyContentType, r.bodyContentLength, nil
			}
			r.nonRepeatableBody = false
			return
		}

		// non-repeatable reader (no retry unless BodyFunc)
		r.bodyFactory = func() (io.ReadCloser, string, int64, error) {
			if rc, ok := reader.(io.ReadCloser); ok {
				return rc, r.bodyContentType, r.bodyContentLength, nil
			}
			return io.NopCloser(reader), r.bodyContentType, r.bodyContentLength, nil
		}
		r.nonRepeatableBody = true
	})
}

// JSONBody marshals JSON, sets Content-Type, retry-safe.
func JSONBody(v any) Option {
	return optionFunc(func(r *request) {
		b, err := json.Marshal(v)
		if err != nil {
			// store as non-repeatable error via factory so doOnce gets it
			r.bodyFactory = func() (io.ReadCloser, string, int64, error) {
				return nil, "", -1, err
			}
			r.nonRepeatableBody = true
			return
		}
		r.bodyContentType = JSON
		r.bodyContentLength = int64(len(b))
		BodyBytes(b).apply(r)
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

func MaxBodyBytes(n int64) Option {
	return optionFunc(func(r *request) { r.maxBodyBytes = n })
}

// RetryCondition determines if request should be retried
type RetryCondition func(resp *Response, err error) bool

// Retry sets retry count and base/max backoff (ms). Optional condition.
func Retry(count int, baseBackoffMs int, maxBackoffMs int, conditions ...RetryCondition) Option {
	return optionFunc(func(r *request) {
		r.retry = count
		r.retryBase = time.Duration(baseBackoffMs) * time.Millisecond
		r.retryMax = time.Duration(maxBackoffMs) * time.Millisecond
		if len(conditions) > 0 {
			r.retryWhen = conditions[0]
		}
	})
}

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

func When5xx() RetryCondition {
	return func(resp *Response, err error) bool {
		if err != nil || resp == nil {
			return true
		}
		return resp.StatusCode >= 500
	}
}

func WhenErr(fn func(err error) bool) RetryCondition {
	return func(resp *Response, err error) bool {
		return err != nil && fn(err)
	}
}

func When(fn func(resp *Response, err error) bool) RetryCondition { return fn }

// ExpectStatus sets expected status codes (O(1))
func ExpectStatus(codes ...int) Option {
	return optionFunc(func(r *request) {
		r.expectStatusList = append(r.expectStatusList[:0], codes...)
		r.expectStatusSet = make(map[int]struct{}, len(codes))
		for _, c := range codes {
			r.expectStatusSet[c] = struct{}{}
		}
	})
}

var okStatusCodes = []int{200, 201, 202, 203, 204, 205, 206, 207, 208, 226}

func ExpectOK() Option { return ExpectStatus(okStatusCodes...) }

func Timeout(ms int) Option {
	return optionFunc(func(r *request) {
		r.timeout = time.Duration(ms) * time.Millisecond
	})
}

// Query helpers (multi-value supported)
func QuerySet(key, value string) Option {
	return optionFunc(func(r *request) { r.query.Set(key, value) })
}
func QueryAdd(key, value string) Option {
	return optionFunc(func(r *request) { r.query.Add(key, value) })
}
func QueryValues(v url.Values) Option {
	return optionFunc(func(r *request) {
		for k, vs := range v {
			for _, x := range vs {
				r.query.Add(k, x)
			}
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

func ContentType(ct string) HeaderOption {
	return func(r *request) {
		r.headers["Content-Type"] = ct
		r.bodyContentType = ct
	}
}
func Accept(ct string) HeaderOption {
	return func(r *request) { r.headers["Accept"] = ct }
}
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

func (c *Client) Do(ctx context.Context, method, path string, opts ...Option) (*Response, error) {
	return c.do(ctx, method, path, opts...)
}

// Parallel
type ParallelResult struct {
	Response *Response
	Error    error
	Index    int
}

type ParallelRequest struct {
	Method string
	Path   string
	Opts   []Option
}

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

func (c *Client) ParallelGet(ctx context.Context, paths []string, opts ...Option) []ParallelResult {
	requests := make([]ParallelRequest, len(paths))
	for i, path := range paths {
		requests[i] = ParallelRequest{Method: http.MethodGet, Path: path, Opts: opts}
	}
	return c.Parallel(ctx, requests)
}

func (c *Client) do(ctx context.Context, method, path string, opts ...Option) (*Response, error) {
	c.ensureHTTP()

	r := getRequest()
	defer putRequest(r)

	for _, opt := range opts {
		opt.apply(r)
	}

	// attach multipart => make bodyFactory (streaming) per attempt
	if r.multipart != nil {
		r.bodyFactory = func() (io.ReadCloser, string, int64, error) {
			return buildMultipartStream(r.multipart)
		}
		// check repeatability for retry
		if !multipartRepeatable(r.multipart) {
			r.nonRepeatableBody = true
		}
	}

	maxAttempts := r.retry + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	// If retry requested but body is non-repeatable and no safe factory => error early
	if r.retry > 0 && r.nonRepeatableBody {
		return nil, ErrNonRepeatable
	}

	var lastErr error
	var lastResp *Response

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt, r.retryBase, r.retryMax); err != nil {
				return nil, err
			}
		}

		resp, err := c.doOnce(ctx, method, path, r)
		lastResp, lastErr = resp, err

		// ExpectStatus
		if err == nil && resp != nil && r.expectStatusSet != nil {
			if _, ok := r.expectStatusSet[resp.StatusCode]; !ok {
				lastErr = &StatusError{
					StatusCode: resp.StatusCode,
					Status:     http.StatusText(resp.StatusCode),
					Body:       resp.Body,
				}
			}
		}

		// decide retry
		if attempt < maxAttempts-1 {
			if r.retryWhen != nil {
				if r.retryWhen(resp, lastErr) {
					continue
				}
			} else if lastErr != nil {
				continue
			}
		}

		return resp, lastErr
	}

	return lastResp, lastErr
}

func sleepBackoff(ctx context.Context, attempt int, base, max time.Duration) error {
	if base <= 0 {
		return nil
	}
	// exponential: base * 2^(attempt-1)
	exp := float64(base) * math.Pow(2, float64(attempt-1))
	d := time.Duration(exp)

	// cap
	if max > 0 && d > max {
		d = max
	}

	// jitter range: [0.5..1.5)
	j := 0.5 + jitter01()
	d = time.Duration(float64(d) * j)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (c *Client) doOnce(ctx context.Context, method, path string, r *request) (*Response, error) {
	if r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}

	fullURL, err := c.buildURL(path, r.query)
	if err != nil {
		return nil, err
	}

	var (
		body          io.ReadCloser
		contentType   string
		contentLength int64 = -1
	)

	if r.bodyFactory != nil {
		body, contentType, contentLength, err = r.bodyFactory()
		if err != nil {
			return nil, err
		}
	} else {
		body = nil
		contentType = r.bodyContentType
		contentLength = r.bodyContentLength
	}

	var reqBody io.Reader
	if body != nil {
		defer body.Close()
		reqBody = body
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reqBody)
	if err != nil {
		return nil, err
	}

	// apply defaults (client-level)
	for k, v := range c.defaultHeaders {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	if c.userAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if c.basicUser != "" {
		req.SetBasicAuth(c.basicUser, c.basicPass)
	}

	// per-request headers
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}

	// content-type from bodyFactory overrides header if not set explicitly
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}

	// content-length for known bodies (non-multipart)
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}

	if c.requestID != nil {
		req.Header.Set("X-Request-ID", c.requestID())
	}

	for _, fn := range c.onRequest {
		fn(req)
	}

	raw, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer raw.Body.Close()

	for _, fn := range c.onResponse {
		fn(raw)
	}

	out := &Response{
		StatusCode: raw.StatusCode,
		Headers:    raw.Header,
	}

	// read/copy with guard
	if r.output != nil {
		n, err := copyWithLimit(r.output, raw.Body, r.maxBodyBytes)
		if err != nil {
			return nil, fmt.Errorf("copy response body: %w", err)
		}
		out.Written = n
	} else {
		b, err := readAllWithLimit(raw.Body, r.maxBodyBytes)
		if err != nil {
			return nil, fmt.Errorf("read response body: %w", err)
		}
		out.Body = b
	}

	for _, fn := range c.onAfter {
		fn(out, raw)
	}

	return out, nil
}

// copyBuffered uses pooled buffer for efficient copying
func copyBuffered(dst io.Writer, src io.Reader) (int64, error) {
	buf := getCopyBuf()
	defer putCopyBuf(buf)
	return io.CopyBuffer(dst, src, buf)
}

func copyWithLimit(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	if limit <= 0 {
		return copyBuffered(dst, src)
	}
	// allow one extra byte to detect overflow
	lr := io.LimitReader(src, limit+1)
	n, err := copyBuffered(dst, lr)
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, &BodyTooLargeError{Limit: limit}
	}
	return n, nil
}

func readAllWithLimit(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		buf := getBuffer()
		defer putBuffer(buf)
		if _, err := buf.ReadFrom(r); err != nil {
			return nil, err
		}
		out := make([]byte, buf.Len())
		copy(out, buf.Bytes())
		return out, nil
	}

	lr := io.LimitReader(r, limit+1)
	buf := getBuffer()
	defer putBuffer(buf)

	if _, err := buf.ReadFrom(lr); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > limit {
		return nil, &BodyTooLargeError{Limit: limit}
	}

	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}

// multipart streaming (retry-safe only if sources are repeatable)
func multipartRepeatable(m *Multipart) bool {
	for _, f := range m.Files {
		if f.Open != nil {
			continue
		}
		if _, ok := f.Reader.(io.ReadSeeker); ok {
			continue
		}
		// non repeatable
		return false
	}
	return true
}

// buildMultipartStream returns a fresh body each call.
// It streams via io.Pipe to avoid buffering whole payload in RAM.
func buildMultipartStream(m *Multipart) (io.ReadCloser, string, int64, error) {
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)

	go func() {
		defer func() {
			_ = w.Close()
			_ = pw.Close()
		}()

		// fields first (either order ok; keeping deterministic helps debugging)
		for k, v := range m.Fields {
			if err := w.WriteField(k, v); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}

		for _, f := range m.Files {
			filename := f.Path
			if filename == "" {
				filename = filepath.Base(f.Name)
			}

			part, err := w.CreateFormFile(f.Name, filename)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}

			var src io.ReadCloser
			if f.Open != nil {
				src, err = f.Open()
				if err != nil {
					_ = pw.CloseWithError(err)
					return
				}
			} else if rs, ok := f.Reader.(io.ReadSeeker); ok {
				if _, err := rs.Seek(0, io.SeekStart); err != nil {
					_ = pw.CloseWithError(err)
					return
				}
				if rc, ok := f.Reader.(io.ReadCloser); ok {
					src = rc
				} else {
					src = io.NopCloser(f.Reader)
				}
			} else {
				// cannot stream repeatably; still stream once
				if rc, ok := f.Reader.(io.ReadCloser); ok {
					src = rc
				} else {
					src = io.NopCloser(f.Reader)
				}
			}

			_, err = copyBuffered(part, src)
			_ = src.Close()
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
	}()

	// content length unknown for streamed multipart
	return pr, w.FormDataContentType(), -1, nil
}

func (c *Client) buildURL(path string, q url.Values) (string, error) {
	// Parse/resolve base + path
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

	// Merge query (multi-values)
	if len(q) > 0 {
		existing := u.Query()
		for k, vs := range q {
			// preserve multi
			for _, v := range vs {
				existing.Add(k, v)
			}
		}
		u.RawQuery = existing.Encode()
	}

	return u.String(), nil
}
