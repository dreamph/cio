package cio

import (
	"bytes"
	"compress/gzip"
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
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
)

func newRequest() *request {
	return &request{
		bodyContentLength: -1,
	}
}

// ensureHeaders lazily initializes headers map
func (r *request) ensureHeaders() map[string]string {
	if r.headers == nil {
		r.headers = make(map[string]string, 8)
	}
	return r.headers
}

// ensureQuery lazily initializes query map
func (r *request) ensureQuery() url.Values {
	if r.query == nil {
		r.query = make(url.Values, 4)
	}
	return r.query
}

// Errors
var (
	ErrUnexpectedStatus = errors.New("unexpected status code")
	ErrNoCookieJar      = errors.New("cookie jar not enabled, use WithCookieJar()")
	ErrBodyTooLarge     = errors.New("response body exceeds MaxBodyBytes")
	ErrNonRepeatable    = errors.New("request body is non-repeatable; retry is not supported without BodyFunc/seekable body")
	ErrStopStream       = errors.New("stop stream") // graceful stop for OnStream/OnStreamRaw
	ErrRateLimited      = errors.New("rate limit exceeded")
	ErrCircuitOpen      = errors.New("circuit breaker is open")
)

// JitterType determines jitter strategy for retries
type JitterType int

const (
	JitterFull         JitterType = iota // Full jitter: [0, backoff)
	JitterEqual                          // Equal jitter: backoff/2 + [0, backoff/2)
	JitterDecorrelated                   // Decorrelated: [base, prev * 3)
)

// RateLimiter implements token bucket rate limiting
type RateLimiter struct {
	rate     float64   // tokens per second
	burst    int64     // max tokens
	tokens   float64   // current tokens
	lastTime time.Time // last token update
	mu       sync.Mutex
}

// NewRateLimiter creates a rate limiter with given rate (req/s) and burst size
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	return &RateLimiter{
		rate:     rate,
		burst:    int64(burst),
		tokens:   float64(burst),
		lastTime: time.Now(),
	}
}

// Allow checks if a request is allowed, blocks until allowed or context cancelled
func (rl *RateLimiter) Allow(ctx context.Context) error {
	for {
		rl.mu.Lock()

		now := time.Now()
		elapsed := now.Sub(rl.lastTime).Seconds()
		rl.tokens = math.Min(float64(rl.burst), rl.tokens+elapsed*rl.rate)
		rl.lastTime = now

		if rl.tokens >= 1 {
			rl.tokens--
			rl.mu.Unlock()
			return nil
		}

		waitSeconds := (1 - rl.tokens) / rl.rate
		rl.mu.Unlock()

		if waitSeconds <= 0 {
			// recalc loop
			continue
		}

		waitTime := time.Duration(waitSeconds * float64(time.Second))
		t := time.NewTimer(waitTime)

		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
			t.Stop()
			// loop เพื่อคำนวณ token ใหม่อีกรอบ
		}
	}
}

// TryAllow checks if a request is allowed without blocking
func (rl *RateLimiter) TryAllow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.lastTime).Seconds()
	rl.tokens = math.Min(float64(rl.burst), rl.tokens+elapsed*rl.rate)
	rl.lastTime = now

	if rl.tokens >= 1 {
		rl.tokens--
		return true
	}
	return false
}

// CircuitState represents circuit breaker state
type CircuitState int32

const (
	CircuitClosed CircuitState = iota
	CircuitOpen
	CircuitHalfOpen
)

// CircuitBreaker implements the circuit breaker pattern
type CircuitBreaker struct {
	failureThreshold int64         // failures before opening
	successThreshold int64         // successes to close from half-open
	timeout          time.Duration // time in open state before half-open

	failures  int64
	successes int64
	state     int32 // atomic CircuitState
	lastFail  time.Time
	mu        sync.Mutex
}

// NewCircuitBreaker creates a circuit breaker
func NewCircuitBreaker(failureThreshold, successThreshold int, timeout time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		failureThreshold: int64(failureThreshold),
		successThreshold: int64(successThreshold),
		timeout:          timeout,
	}
}

// Allow checks if request is allowed through circuit breaker
func (cb *CircuitBreaker) Allow() error {
	state := CircuitState(atomic.LoadInt32(&cb.state))

	switch state {
	case CircuitOpen:
		cb.mu.Lock()
		if time.Since(cb.lastFail) > cb.timeout {
			atomic.StoreInt32(&cb.state, int32(CircuitHalfOpen))
			cb.successes = 0
			cb.mu.Unlock()
			return nil
		}
		cb.mu.Unlock()
		return ErrCircuitOpen
	case CircuitHalfOpen:
		return nil
	default: // Closed
		return nil
	}
}

// RecordSuccess records a successful request
func (cb *CircuitBreaker) RecordSuccess() {
	state := CircuitState(atomic.LoadInt32(&cb.state))

	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch state {
	case CircuitHalfOpen:
		cb.successes++
		if cb.successes >= cb.successThreshold {
			atomic.StoreInt32(&cb.state, int32(CircuitClosed))
			cb.failures = 0
		}
	case CircuitClosed:
		cb.failures = 0
	}
}

// RecordFailure records a failed request
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	state := CircuitState(atomic.LoadInt32(&cb.state))

	switch state {
	case CircuitHalfOpen:
		atomic.StoreInt32(&cb.state, int32(CircuitOpen))
		cb.lastFail = time.Now()
	case CircuitClosed:
		cb.failures++
		if cb.failures >= cb.failureThreshold {
			atomic.StoreInt32(&cb.state, int32(CircuitOpen))
			cb.lastFail = time.Now()
		}
	}
}

// State returns current circuit state
func (cb *CircuitBreaker) State() CircuitState {
	return CircuitState(atomic.LoadInt32(&cb.state))
}

// Reset resets the circuit breaker to closed state
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	atomic.StoreInt32(&cb.state, int32(CircuitClosed))
	cb.failures = 0
	cb.successes = 0
}

// StatusError represents an HTTP status error with request context for debugging
type StatusError struct {
	StatusCode int
	Status     string
	Body       []byte
	Method     string // HTTP method (GET, POST, etc.)
	URL        string // Full request URL
}

func (e *StatusError) Error() string {
	if e.Method != "" && e.URL != "" {
		return fmt.Sprintf("unexpected status %d: %s [%s %s]", e.StatusCode, e.Status, e.Method, e.URL)
	}
	return fmt.Sprintf("unexpected status %d: %s", e.StatusCode, e.Status)
}

type BodyTooLargeError struct {
	Limit int64
}

func (e *BodyTooLargeError) Error() string {
	return fmt.Sprintf("%v: limit=%d", ErrBodyTooLarge, e.Limit)
}

// JSON codec function types
type (
	JSONMarshal   func(v any) ([]byte, error)
	JSONUnmarshal func(data []byte, v any) error
)

// Interceptors
type RequestInterceptor func(*http.Request)
type ResponseInterceptor func(*http.Response)
type AfterReadInterceptor func(resp *Response, raw *http.Response)
type MetricsInterceptor func(method, path string, status int, duration time.Duration, err error)

// Client
type Client struct {
	http          *http.Client
	baseURL       string
	parsedBaseURL *url.URL // cached parsed base URL

	onRequest  []RequestInterceptor
	onResponse []ResponseInterceptor
	onAfter    []AfterReadInterceptor
	onMetrics  []MetricsInterceptor

	requestID func() string

	// defaults
	defaultHeaders map[string]string
	defaultTimeout time.Duration
	userAgent      string
	basicUser      string
	basicPass      string

	// json codec
	jsonMarshal   JSONMarshal
	jsonUnmarshal JSONUnmarshal

	// rate limiting & circuit breaker
	rateLimiter    *RateLimiter
	circuitBreaker *CircuitBreaker
}

type ClientOption func(*Client)

func BaseURL(u string) ClientOption {
	return func(c *Client) {
		c.baseURL = u
		if u != "" {
			c.parsedBaseURL, _ = url.Parse(u)
		}
	}
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

// OnMetrics adds a metrics callback that fires after each request completes.
// Receives method, path (without query), status code, duration, and error (if any).
func OnMetrics(fn MetricsInterceptor) ClientOption {
	return func(c *Client) { c.onMetrics = append(c.onMetrics, fn) }
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

// WithJSONCodec sets custom JSON marshal/unmarshal functions
// Example: cio.WithJSONCodec(sonic.Marshal, sonic.Unmarshal)
func WithJSONCodec(marshal JSONMarshal, unmarshal JSONUnmarshal) ClientOption {
	return func(c *Client) {
		c.jsonMarshal = marshal
		c.jsonUnmarshal = unmarshal
	}
}

// WithRateLimiter sets a rate limiter for all requests
func WithRateLimiter(rl *RateLimiter) ClientOption {
	return func(c *Client) { c.rateLimiter = rl }
}

// WithRateLimit creates and sets a rate limiter (requests per second, burst size)
func WithRateLimit(rps float64, burst int) ClientOption {
	return func(c *Client) { c.rateLimiter = NewRateLimiter(rps, burst) }
}

// WithCircuitBreaker sets a circuit breaker for all requests
func WithCircuitBreaker(cb *CircuitBreaker) ClientOption {
	return func(c *Client) { c.circuitBreaker = cb }
}

// WithCircuit creates and sets a circuit breaker (failure threshold, success threshold, timeout)
func WithCircuit(failures, successes int, timeout time.Duration) ClientOption {
	return func(c *Client) { c.circuitBreaker = NewCircuitBreaker(failures, successes, timeout) }
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

func DisableRedirects() ClientOption { return WithRedirects(0) }

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

// DisableKeepAlive disables HTTP keep-alive / connection reuse.
// Each request will open a new TCP connection.
func DisableKeepAlive() ClientOption {
	return func(c *Client) {
		c.ensureHTTP()

		tr, ok := c.http.Transport.(*http.Transport)
		if !ok || tr == nil {
			tr = DefaultTransport()
		}

		tr.DisableKeepAlives = true
		tr.MaxIdleConns = 0
		tr.MaxIdleConnsPerHost = 0

		c.http.Transport = tr
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
		http:          &http.Client{Transport: DefaultTransport()},
		jsonMarshal:   json.Marshal,
		jsonUnmarshal: json.Unmarshal,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.ensureHTTP()
	return c
}

// Clone creates a new Client with the same settings, then applies additional options.
// The new client shares the same http.Client (and thus connection pool) by default.
// Use HTTPClient() or Transport() option to use a separate connection pool.
func (c *Client) Clone(opts ...ClientOption) *Client {
	clone := &Client{
		http:           c.http,
		baseURL:        c.baseURL,
		parsedBaseURL:  c.parsedBaseURL,
		requestID:      c.requestID,
		userAgent:      c.userAgent,
		basicUser:      c.basicUser,
		basicPass:      c.basicPass,
		defaultTimeout: c.defaultTimeout,
		jsonMarshal:    c.jsonMarshal,
		jsonUnmarshal:  c.jsonUnmarshal,
		rateLimiter:    c.rateLimiter,
		circuitBreaker: c.circuitBreaker,
	}

	// Copy slices
	if len(c.onRequest) > 0 {
		clone.onRequest = make([]RequestInterceptor, len(c.onRequest))
		copy(clone.onRequest, c.onRequest)
	}
	if len(c.onResponse) > 0 {
		clone.onResponse = make([]ResponseInterceptor, len(c.onResponse))
		copy(clone.onResponse, c.onResponse)
	}
	if len(c.onAfter) > 0 {
		clone.onAfter = make([]AfterReadInterceptor, len(c.onAfter))
		copy(clone.onAfter, c.onAfter)
	}
	if len(c.onMetrics) > 0 {
		clone.onMetrics = make([]MetricsInterceptor, len(c.onMetrics))
		copy(clone.onMetrics, c.onMetrics)
	}

	// Copy map
	if len(c.defaultHeaders) > 0 {
		clone.defaultHeaders = make(map[string]string, len(c.defaultHeaders))
		for k, v := range c.defaultHeaders {
			clone.defaultHeaders[k] = v
		}
	}

	// Apply new options
	for _, opt := range opts {
		opt(clone)
	}

	return clone
}

// WithTimeout sets default timeout for all requests (can be overridden per-request with Timeout())
func WithTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.defaultTimeout = d }
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

// TraceInfo contains detailed timing information for a request
type TraceInfo struct {
	DNSLookup     time.Duration
	ConnectTime   time.Duration
	TLSHandshake  time.Duration
	FirstByteTime time.Duration
	TotalTime     time.Duration
	RemoteAddr    string
	LocalAddr     string
	WasReused     bool
}

// Response
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Written    int64 // bytes written when using OutputStream

	// Extended info (populated when requested)
	Ctx         context.Context // request context
	RawRequest  *http.Request   // original request (when WithRawCapture)
	RawResponse *http.Response  // original response (when WithRawCapture)
	Trace       *TraceInfo      // timing info (when WithTrace)

	client *Client // internal reference for JSON decoding
}

func (r *Response) OK() bool { return r.StatusCode >= 200 && r.StatusCode < 300 }
func (r *Response) String() string {
	if r.Body == nil {
		return ""
	}
	return string(r.Body)
}

// Context returns the request context
func (r *Response) Context() context.Context {
	if r.Ctx != nil {
		return r.Ctx
	}
	return context.Background()
}

// Cookies returns cookies from Set-Cookie headers
func (r *Response) Cookies() []*http.Cookie {
	if r.RawResponse != nil {
		return r.RawResponse.Cookies()
	}
	// Parse from headers manually if RawResponse not captured
	header := http.Header{"Set-Cookie": r.Headers.Values("Set-Cookie")}
	resp := &http.Response{Header: header}
	return resp.Cookies()
}

// Location returns the Location header URL (for redirects)
func (r *Response) Location() string {
	return r.Headers.Get("Location")
}

// LocationURL parses Location header as *url.URL
func (r *Response) LocationURL() (*url.URL, error) {
	loc := r.Headers.Get("Location")
	if loc == "" {
		return nil, errors.New("no Location header")
	}
	return url.Parse(loc)
}

// Json decodes response body using the client's JSON unmarshal function
func (r *Response) Json(v any) error {
	if r.client != nil && r.client.jsonUnmarshal != nil {
		return r.client.jsonUnmarshal(r.Body, v)
	}
	return json.Unmarshal(r.Body, v)
}

// Json decodes response body into type T using the client's JSON codec
func Json[T any](r *Response) (T, error) {
	var v T
	err := r.Json(&v)
	return v, err
}

// ContentLength returns Content-Length header value, -1 if unknown
func (r *Response) ContentLength() int64 {
	if v := r.Headers.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return -1
}

// ContentType returns Content-Type header value (e.g. "application/json")
func (r *Response) ContentType() string {
	return r.Headers.Get("Content-Type")
}

// ETag returns ETag header value (e.g. `"abc123"` or `W/"abc123"`)
func (r *Response) ETag() string {
	return r.Headers.Get("ETag")
}

// LastModified returns Last-Modified header as time.Time, zero time if not present or invalid
func (r *Response) LastModified() time.Time {
	if v := r.Headers.Get("Last-Modified"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			return t
		}
	}
	return time.Time{}
}

// AcceptRanges returns true if server supports range requests (Accept-Ranges: bytes)
func (r *Response) AcceptRanges() bool {
	return r.Headers.Get("Accept-Ranges") == "bytes"
}

// IsJSON returns true if Content-Type indicates JSON
func (r *Response) IsJSON() bool {
	ct := r.ContentType()
	return ct == JSON || ct == "application/json; charset=utf-8" ||
		len(ct) > 16 && ct[:16] == "application/json"
}

// IsXML returns true if Content-Type indicates XML
func (r *Response) IsXML() bool {
	ct := r.ContentType()
	return ct == XML || ct == "text/xml" ||
		len(ct) > 15 && ct[:15] == "application/xml" ||
		len(ct) > 8 && ct[:8] == "text/xml"
}

// IsText returns true if Content-Type indicates text
func (r *Response) IsText() bool {
	ct := r.ContentType()
	return len(ct) >= 5 && ct[:5] == "text/"
}

// IsNotModified returns true if status is 304 Not Modified
func (r *Response) IsNotModified() bool {
	return r.StatusCode == http.StatusNotModified
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
		r.ensureHeaders()[k] = v
	}
	for k, v := range cfg.Query {
		r.ensureQuery().Set(k, v)
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
	jsonValue         any // deferred JSON marshaling (uses client codec)

	// multipart (built via bodyFactory per attempt)
	multipart *Multipart

	// retry
	retry      int
	retryBase  time.Duration
	retryMax   time.Duration
	retryWhen  RetryCondition
	onRetry    func(attempt int, err error)
	jitterType JitterType

	// expectations
	expectStatusList []int
	expectStatusSet  map[int]struct{}

	// response guard
	maxBodyBytes int64

	// streaming
	onStream    func(line []byte) error  // line-based callback (SSE/NDJSON)
	onStreamRaw func(chunk []byte) error // raw chunk callback

	// debug
	debugWriter io.Writer

	// extended options
	rawCapture     bool   // capture RawRequest/RawResponse
	traceEnabled   bool   // enable TraceInfo
	rawBody        bool   // return io.ReadCloser instead of []byte
	gzipRequest    bool   // gzip compress request body
	decompressResp bool   // manual decompress response
	basicUser      string // per-request basic auth
	basicPass      string
	hostHeader     string // override Host header
	dumpRequest    bool   // dump full request
	dumpResponse   bool   // dump full response
	dumpWriter     io.Writer
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
// Note: For concurrent usage or retry safety, prefer BodyFunc with a factory function.
func BodyReader(reader io.Reader) Option {
	return optionFunc(func(r *request) {
		// clear bytes
		r.bodyBytes = nil

		if rs, ok := reader.(io.ReadSeeker); ok {
			// Wrap in a factory that creates a fresh wrapper each time
			// to avoid shared state issues with concurrent requests
			r.bodyFactory = func() (io.ReadCloser, string, int64, error) {
				if _, err := rs.Seek(0, io.SeekStart); err != nil {
					return nil, "", -1, err
				}
				// Create a new wrapper each time to avoid shared ReadCloser state
				return io.NopCloser(&seekerWrapper{rs: rs}), r.bodyContentType, r.bodyContentLength, nil
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

// seekerWrapper wraps a ReadSeeker to provide independent read position tracking
type seekerWrapper struct {
	rs io.ReadSeeker
}

func (w *seekerWrapper) Read(p []byte) (int, error) {
	return w.rs.Read(p)
}

// JSONBody sets JSON body. Marshaling uses client's codec (deferred until request execution).
func JSONBody(v any) Option {
	return optionFunc(func(r *request) {
		r.jsonValue = v
		r.bodyContentType = JSON
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

// OnStream sets a line-based streaming callback for SSE/NDJSON.
// Each line (without \n) is passed to the callback.
// Return ErrStopStream to stop gracefully, or any other error to abort.
func OnStream(fn func(line []byte) error) Option {
	return optionFunc(func(r *request) { r.onStream = fn })
}

// OnStreamRaw sets a raw chunk streaming callback.
// Raw chunks are passed as-is from the response body.
// Return ErrStopStream to stop gracefully, or any other error to abort.
func OnStreamRaw(fn func(chunk []byte) error) Option {
	return optionFunc(func(r *request) { r.onStreamRaw = fn })
}

// Debug enables logging of request and response details.
// Output format: --> METHOD URL \n <-- STATUS (duration)
func Debug(w io.Writer) Option {
	return optionFunc(func(r *request) {
		if w == nil {
			r.debugWriter = os.Stdout
		} else {
			r.debugWriter = w
		}
	})
}

// DebugStdout enables debug logging to stdout
func DebugStdout() Option { return Debug(os.Stdout) }

// Range sets the Range header for partial content requests.
// Use end=-1 for "start to end of file": Range(1000, -1) -> "bytes=1000-"
func Range(start, end int64) Option {
	return optionFunc(func(r *request) {
		if end < 0 {
			r.ensureHeaders()["Range"] = fmt.Sprintf("bytes=%d-", start)
		} else {
			r.ensureHeaders()["Range"] = fmt.Sprintf("bytes=%d-%d", start, end)
		}
	})
}

// IfNoneMatch sets If-None-Match header for conditional requests (caching).
// Server returns 304 Not Modified if ETag matches.
func IfNoneMatch(etag string) Option {
	return optionFunc(func(r *request) {
		r.ensureHeaders()["If-None-Match"] = etag
	})
}

// IfModifiedSince sets If-Modified-Since header for conditional requests.
// Server returns 304 Not Modified if resource hasn't changed since time t.
func IfModifiedSince(t time.Time) Option {
	return optionFunc(func(r *request) {
		r.ensureHeaders()["If-Modified-Since"] = t.UTC().Format(http.TimeFormat)
	})
}

// IfMatch sets If-Match header for conditional requests (optimistic locking).
// Server returns 412 Precondition Failed if ETag doesn't match.
func IfMatch(etag string) Option {
	return optionFunc(func(r *request) {
		r.ensureHeaders()["If-Match"] = etag
	})
}

// FormURLEncoded sets application/x-www-form-urlencoded body
func FormURLEncoded(data map[string]string) Option {
	return optionFunc(func(r *request) {
		values := url.Values{}
		for k, v := range data {
			values.Set(k, v)
		}
		encoded := values.Encode()
		b := []byte(encoded)

		r.bodyBytes = b
		r.bodyContentType = Form
		r.bodyContentLength = int64(len(b))
		r.bodyFactory = func() (io.ReadCloser, string, int64, error) {
			return io.NopCloser(bytes.NewReader(b)), Form, int64(len(b)), nil
		}
		r.nonRepeatableBody = false
	})
}

// FormURLEncodedValues sets application/x-www-form-urlencoded body from url.Values (supports multi-value)
func FormURLEncodedValues(data url.Values) Option {
	return optionFunc(func(r *request) {
		encoded := data.Encode()
		b := []byte(encoded)

		r.bodyBytes = b
		r.bodyContentType = Form
		r.bodyContentLength = int64(len(b))
		r.bodyFactory = func() (io.ReadCloser, string, int64, error) {
			return io.NopCloser(bytes.NewReader(b)), Form, int64(len(b)), nil
		}
		r.nonRepeatableBody = false
	})
}

// XMLBody sets XML body (raw bytes). Use with your preferred XML encoder.
// Example: cio.XMLBody(xmlBytes)
func XMLBody(data []byte) Option {
	return optionFunc(func(r *request) {
		r.bodyBytes = data
		r.bodyContentType = XML
		r.bodyContentLength = int64(len(data))
		r.bodyFactory = func() (io.ReadCloser, string, int64, error) {
			return io.NopCloser(bytes.NewReader(data)), XML, int64(len(data)), nil
		}
		r.nonRepeatableBody = false
	})
}

// BasicAuth sets per-request basic authentication (overrides client-level)
func BasicAuth(user, pass string) Option {
	return optionFunc(func(r *request) {
		r.basicUser = user
		r.basicPass = pass
	})
}

// HostHeader overrides the Host header
func HostHeader(host string) Option {
	return optionFunc(func(r *request) { r.hostHeader = host })
}

// WithRawCapture enables capturing RawRequest and RawResponse in Response
func WithRawCapture() Option {
	return optionFunc(func(r *request) { r.rawCapture = true })
}

// WithTrace enables detailed timing information in Response.Trace
func WithTrace() Option {
	return optionFunc(func(r *request) { r.traceEnabled = true })
}

// Gzip compresses the request body with gzip
func Gzip() Option {
	return optionFunc(func(r *request) { r.gzipRequest = true })
}

// Decompress manually decompresses gzip/deflate response (normally handled by transport)
func Decompress() Option {
	return optionFunc(func(r *request) { r.decompressResp = true })
}

// DumpRequest dumps the full HTTP request to the writer
func DumpRequest(w io.Writer) Option {
	return optionFunc(func(r *request) {
		r.dumpRequest = true
		r.dumpWriter = w
	})
}

// DumpResponse dumps the full HTTP response to the writer
func DumpResponse(w io.Writer) Option {
	return optionFunc(func(r *request) {
		r.dumpResponse = true
		r.dumpWriter = w
	})
}

// Dump dumps both request and response
func Dump(w io.Writer) Option {
	return optionFunc(func(r *request) {
		r.dumpRequest = true
		r.dumpResponse = true
		r.dumpWriter = w
	})
}

// WithJitter sets jitter type for retry backoff
func WithJitter(jt JitterType) Option {
	return optionFunc(func(r *request) { r.jitterType = jt })
}

// CacheControl sets Cache-Control header
func CacheControl(directive string) Option {
	return optionFunc(func(r *request) { r.ensureHeaders()["Cache-Control"] = directive })
}

// DisableCache sets Cache-Control: no-cache
func DisableCache() Option { return CacheControl("no-cache") }

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

// OnRetry sets a callback that fires before each retry attempt.
// attempt starts at 1 (first retry), err is the error from previous attempt.
func OnRetry(fn func(attempt int, err error)) Option {
	return optionFunc(func(r *request) { r.onRetry = fn })
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
	return optionFunc(func(r *request) { r.ensureQuery().Set(key, value) })
}
func QueryAdd(key, value string) Option {
	return optionFunc(func(r *request) { r.ensureQuery().Add(key, value) })
}
func QueryValues(v url.Values) Option {
	return optionFunc(func(r *request) {
		for k, vs := range v {
			for _, x := range vs {
				r.ensureQuery().Add(k, x)
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
		r.ensureHeaders()["Content-Type"] = ct
		r.bodyContentType = ct
	}
}
func Accept(ct string) HeaderOption {
	return func(r *request) { r.ensureHeaders()["Accept"] = ct }
}
func Header(key, value string) HeaderOption {
	return func(r *request) { r.ensureHeaders()[key] = value }
}

const bearerPrefix = "Bearer "

func Bearer(token string) HeaderOption {
	return func(r *request) { r.ensureHeaders()["Authorization"] = bearerPrefix + token }
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

	r := newRequest()

	for _, opt := range opts {
		opt.apply(r)
	}

	// Rate limiting
	if c.rateLimiter != nil {
		if err := c.rateLimiter.Allow(ctx); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRateLimited, err)
		}
	}

	// Circuit breaker check
	if c.circuitBreaker != nil {
		if err := c.circuitBreaker.Allow(); err != nil {
			return nil, err
		}
	}

	// Build full URL early for error reporting
	fullURL, err := c.buildURL(path, r.query)
	if err != nil {
		return nil, err
	}

	// Handle deferred JSON marshaling with client's codec
	if r.jsonValue != nil {
		b, err := c.jsonMarshal(r.jsonValue)
		if err != nil {
			return nil, fmt.Errorf("json marshal: %w", err)
		}
		r.bodyBytes = b
		r.bodyContentLength = int64(len(b))
		r.bodyFactory = func() (io.ReadCloser, string, int64, error) {
			return io.NopCloser(bytes.NewReader(b)), JSON, int64(len(b)), nil
		}
		r.nonRepeatableBody = false
	}

	// attach multipart => make bodyFactory (streaming) per attempt with context awareness
	if r.multipart != nil {
		m := r.multipart // capture for closure
		r.bodyFactory = func() (io.ReadCloser, string, int64, error) {
			return buildMultipartStream(ctx, m)
		}
		// check repeatability for retry
		if !multipartRepeatable(r.multipart) {
			r.nonRepeatableBody = true
		}
	}

	// Gzip compression (wrap bodyFactory ที่สร้างเสร็จแล้ว)
	if r.gzipRequest && r.bodyFactory != nil {
		originalFactory := r.bodyFactory
		r.bodyFactory = gzipBody(originalFactory)
		r.ensureHeaders()["Content-Encoding"] = "gzip"
	}

	maxAttempts := r.retry + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	// If retry requested but body is non-repeatable and no safe factory => error early
	if r.retry > 0 && r.nonRepeatableBody {
		return nil, ErrNonRepeatable
	}

	// Start timing for metrics
	start := time.Now()

	var lastErr error
	var lastResp *Response

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Fire onRetry callback before retry
			if r.onRetry != nil {
				r.onRetry(attempt, lastErr)
			}
			if err := sleepBackoff(ctx, attempt, r.retryBase, r.retryMax); err != nil {
				return nil, err
			}
		}

		// Debug: log request
		reqStart := time.Now()
		if r.debugWriter != nil {
			fmt.Fprintf(r.debugWriter, "--> %s %s\n", method, fullURL)
		}

		resp, err := c.doOnce(ctx, method, fullURL, r)
		lastResp, lastErr = resp, err

		// Debug: log response
		if r.debugWriter != nil {
			if resp != nil {
				fmt.Fprintf(r.debugWriter, "<-- %d %s (%v)\n", resp.StatusCode, http.StatusText(resp.StatusCode), time.Since(reqStart))
			} else if err != nil {
				fmt.Fprintf(r.debugWriter, "<-- ERROR: %v (%v)\n", err, time.Since(reqStart))
			}
		}

		// ExpectStatus - now includes method and URL for debugging
		if err == nil && resp != nil && r.expectStatusSet != nil {
			if _, ok := r.expectStatusSet[resp.StatusCode]; !ok {
				// Limit error body to 1KB to avoid memory bloat
				errBody := resp.Body
				if len(errBody) > 1024 {
					errBody = errBody[:1024]
				}
				lastErr = &StatusError{
					StatusCode: resp.StatusCode,
					Status:     http.StatusText(resp.StatusCode),
					Body:       errBody,
					Method:     method,
					URL:        fullURL,
				}
			}
		}

		// decide retry
		if attempt < maxAttempts-1 {
			if r.retryWhen != nil {
				if r.retryWhen(resp, lastErr) {
					// Record failure for circuit breaker
					if c.circuitBreaker != nil {
						c.circuitBreaker.RecordFailure()
					}
					continue
				}
			} else if lastErr != nil {
				// Record failure for circuit breaker
				if c.circuitBreaker != nil {
					c.circuitBreaker.RecordFailure()
				}
				continue
			}
		}

		// Record success/failure for circuit breaker
		if c.circuitBreaker != nil {
			if lastErr != nil || (resp != nil && resp.StatusCode >= 500) {
				c.circuitBreaker.RecordFailure()
			} else {
				c.circuitBreaker.RecordSuccess()
			}
		}

		// Fire metrics callbacks
		c.fireMetrics(method, path, lastResp, time.Since(start), lastErr)
		return resp, lastErr
	}

	// Record final failure for circuit breaker
	if c.circuitBreaker != nil {
		c.circuitBreaker.RecordFailure()
	}

	// Fire metrics callbacks for final attempt
	c.fireMetrics(method, path, lastResp, time.Since(start), lastErr)
	return lastResp, lastErr
}

// fireMetrics calls all registered metrics interceptors
func (c *Client) fireMetrics(method, path string, resp *Response, duration time.Duration, err error) {
	if len(c.onMetrics) == 0 {
		return
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	for _, fn := range c.onMetrics {
		fn(method, path, status, duration, err)
	}
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

	// jitter range: [0.5..1.5) - mrand is thread-safe since Go 1.20
	j := 0.5 + mrand.Float64()
	d = time.Duration(float64(d) * j)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// doOnce now receives the pre-built fullURL
func (c *Client) doOnce(ctx context.Context, method, fullURL string, r *request) (*Response, error) {
	// Use request timeout if set, otherwise fall back to client default
	timeout := r.timeout
	if timeout == 0 {
		timeout = c.defaultTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	reqStart := time.Now()

	// Trace hook
	var traceInfo *TraceInfo
	if r.traceEnabled {
		ti := &TraceInfo{}
		traceInfo = ti

		var dnsStart, connStart time.Time

		clientTrace := &httptrace.ClientTrace{
			DNSStart: func(info httptrace.DNSStartInfo) {
				dnsStart = time.Now()
			},
			DNSDone: func(info httptrace.DNSDoneInfo) {
				if !dnsStart.IsZero() {
					ti.DNSLookup = time.Since(dnsStart)
				}
			},
			ConnectStart: func(network, addr string) {
				connStart = time.Now()
			},
			ConnectDone: func(network, addr string, err error) {
				if err == nil && !connStart.IsZero() {
					ti.ConnectTime = time.Since(connStart)
					ti.RemoteAddr = addr
				}
			},
			GotConn: func(info httptrace.GotConnInfo) {
				ti.WasReused = info.Reused
				if info.Conn != nil {
					if la := info.Conn.LocalAddr(); la != nil {
						ti.LocalAddr = la.String()
					}
				}
			},
			GotFirstResponseByte: func() {
				ti.FirstByteTime = time.Since(reqStart)
			},
		}

		ctx = httptrace.WithClientTrace(ctx, clientTrace)
	}

	var (
		body          io.ReadCloser
		contentType   string
		contentLength int64 = -1
		err           error
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

	// apply defaults (client-level) - use direct assignment for speed
	for k, v := range c.defaultHeaders {
		if _, exists := req.Header[k]; !exists {
			req.Header[k] = []string{v}
		}
	}
	if c.userAgent != "" {
		if _, exists := req.Header["User-Agent"]; !exists {
			req.Header["User-Agent"] = []string{c.userAgent}
		}
	}

	// Basic auth: per-request overrides client-level
	if r.basicUser != "" {
		req.SetBasicAuth(r.basicUser, r.basicPass)
	} else if c.basicUser != "" {
		req.SetBasicAuth(c.basicUser, c.basicPass)
	}

	// per-request headers (keys are already canonical from our constants)
	for k, v := range r.headers {
		req.Header[k] = []string{v}
	}

	// Host header override
	if r.hostHeader != "" {
		req.Host = r.hostHeader
	}

	// content-type from bodyFactory overrides header if not set explicitly
	if contentType != "" {
		if _, exists := req.Header["Content-Type"]; !exists {
			req.Header["Content-Type"] = []string{contentType}
		}
	}

	// content-length for known bodies (non-multipart)
	if contentLength >= 0 {
		req.ContentLength = contentLength
	}

	if c.requestID != nil {
		req.Header["X-Request-Id"] = []string{c.requestID()}
	}

	for _, fn := range c.onRequest {
		fn(req)
	}

	// Dump request
	if r.dumpRequest && r.dumpWriter != nil {
		dump, _ := httputil.DumpRequestOut(req, true)
		r.dumpWriter.Write(dump)
		r.dumpWriter.Write([]byte("\n"))
	}

	raw, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer raw.Body.Close()

	// Dump response
	if r.dumpResponse && r.dumpWriter != nil {
		dump, _ := httputil.DumpResponse(raw, true)
		r.dumpWriter.Write(dump)
		r.dumpWriter.Write([]byte("\n"))
	}

	for _, fn := range c.onResponse {
		fn(raw)
	}

	// manual decompress (กรณี transport.DisableCompression = true)
	if r.decompressResp {
		if enc := raw.Header.Get("Content-Encoding"); enc == "gzip" {
			gzr, err := decompressReader(raw.Body)
			if err != nil {
				return nil, fmt.Errorf("decompress response: %w", err)
			}
			raw.Body = gzr
			raw.Header.Del("Content-Encoding")
		}
	}

	out := &Response{
		StatusCode: raw.StatusCode,
		Headers:    raw.Header,
		client:     c,
		Ctx:        ctx,
		Trace:      traceInfo,
	}

	if traceInfo != nil {
		traceInfo.TotalTime = time.Since(reqStart)
	}

	// Capture raw request/response if requested
	if r.rawCapture {
		out.RawRequest = req
		out.RawResponse = raw
	}

	// Handle streaming callbacks
	if r.onStream != nil {
		err := streamLines(raw.Body, r.onStream)
		if err != nil && !errors.Is(err, ErrStopStream) {
			return out, fmt.Errorf("stream lines: %w", err)
		}
		return out, nil
	}

	if r.onStreamRaw != nil {
		err := streamRaw(raw.Body, r.onStreamRaw)
		if err != nil && !errors.Is(err, ErrStopStream) {
			return out, fmt.Errorf("stream raw: %w", err)
		}
		return out, nil
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

// streamLines reads lines from reader and calls fn for each line.
// Lines are split by \n, \r\n is handled. Empty lines are passed to fn.
func streamLines(r io.Reader, fn func(line []byte) error) error {
	buf := make([]byte, defaultBufferSize)
	var lineBuf []byte

	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			for {
				idx := bytes.IndexByte(chunk, '\n')
				if idx < 0 {
					lineBuf = append(lineBuf, chunk...)
					break
				}
				line := chunk[:idx]
				if len(lineBuf) > 0 {
					lineBuf = append(lineBuf, line...)
					line = lineBuf
				}
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				if err := fn(line); err != nil {
					return err
				}
				lineBuf = lineBuf[:0]
				chunk = chunk[idx+1:]
			}
		}
		if err != nil {
			if err == io.EOF {
				if len(lineBuf) > 0 {
					if err := fn(lineBuf); err != nil {
						return err
					}
				}
				return nil
			}
			return err
		}
	}
}

// streamRaw reads raw chunks from reader and calls fn for each chunk.
func streamRaw(r io.Reader, fn func(chunk []byte) error) error {
	buf := make([]byte, defaultBufferSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if err := fn(buf[:n]); err != nil {
				return err
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// copyWithLimit copies with optional size limit
func copyWithLimit(dst io.Writer, src io.Reader, limit int64) (int64, error) {
	if limit <= 0 {
		return io.Copy(dst, src)
	}
	lr := io.LimitReader(src, limit+1)
	n, err := io.Copy(dst, lr)
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, &BodyTooLargeError{Limit: limit}
	}
	return n, nil
}

// readAllWithLimit reads response body with optional size limit.
func readAllWithLimit(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}

	lr := io.LimitReader(r, limit+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, &BodyTooLargeError{Limit: limit}
	}
	return data, nil
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
// Now context-aware to prevent goroutine leaks on cancellation.
func buildMultipartStream(ctx context.Context, m *Multipart) (io.ReadCloser, string, int64, error) {
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)

	go func() {
		var err error
		defer func() {
			_ = w.Close()
			if err != nil {
				_ = pw.CloseWithError(err)
			} else {
				_ = pw.Close()
			}
		}()

		// fields first (either order ok; keeping deterministic helps debugging)
		for k, v := range m.Fields {
			// Check context before potentially blocking operations
			select {
			case <-ctx.Done():
				err = ctx.Err()
				return
			default:
			}

			if err = w.WriteField(k, v); err != nil {
				return
			}
		}

		for _, f := range m.Files {
			// Check context before processing each file
			select {
			case <-ctx.Done():
				err = ctx.Err()
				return
			default:
			}

			filename := f.Path
			if filename == "" {
				filename = filepath.Base(f.Name)
			}

			var part io.Writer
			part, err = w.CreateFormFile(f.Name, filename)
			if err != nil {
				return
			}

			var src io.ReadCloser
			if f.Open != nil {
				src, err = f.Open()
				if err != nil {
					return
				}
			} else if rs, ok := f.Reader.(io.ReadSeeker); ok {
				if _, err = rs.Seek(0, io.SeekStart); err != nil {
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

			// Use context-aware copy
			_, err = copyWithContext(ctx, part, src)
			_ = src.Close()
			if err != nil {
				return
			}
		}
	}()

	// content length unknown for streamed multipart
	return pr, w.FormDataContentType(), -1, nil
}

// copyWithContext copies from src to dst with context cancellation support.
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, defaultBufferSize)
	var written int64

	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}

		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = errors.New("invalid write result")
				}
			}
			written += int64(nw)
			if ew != nil {
				return written, ew
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return written, nil
			}
			return written, er
		}
	}
}

func (c *Client) buildURL(path string, q url.Values) (string, error) {
	var u *url.URL

	if c.parsedBaseURL != nil {
		// clone base
		base := *c.parsedBaseURL

		// normalize path join
		base.Path = strings.TrimRight(base.Path, "/")

		if strings.HasPrefix(path, "/") {
			path = "/" + strings.TrimLeft(path, "/")
		} else if path != "" {
			path = "/" + path
		}

		base.Path = base.Path + path
		u = &base

	} else {
		parsed, err := url.Parse(path)
		if err != nil {
			return "", err
		}
		u = parsed
	}

	// merge query
	if len(q) > 0 {
		if u.RawQuery == "" {
			u.RawQuery = q.Encode()
		} else {
			existing := u.Query()
			for k, vs := range q {
				for _, v := range vs {
					existing.Add(k, v)
				}
			}
			u.RawQuery = existing.Encode()
		}
	}

	return u.String(), nil
}

// ==================== File Helpers ====================

// Download downloads a file to the given path
func (c *Client) Download(ctx context.Context, urlPath, filePath string, opts ...Option) (*Response, error) {
	f, err := os.Create(filePath)
	if err != nil {
		return nil, fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	opts = append(opts, OutputStream(f))
	resp, err := c.Get(ctx, urlPath, opts...)
	if err != nil {
		os.Remove(filePath) // cleanup on error
		return nil, err
	}
	if !resp.OK() {
		os.Remove(filePath)
		return resp, fmt.Errorf("download failed: status %d", resp.StatusCode)
	}
	return resp, nil
}

// Upload uploads a file from the given path
func (c *Client) Upload(ctx context.Context, urlPath, filePath, fieldName string, opts ...Option) (*Response, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}

	stat, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat file: %w", err)
	}

	file := File{
		Name: fieldName,
		Path: filepath.Base(filePath),
		Open: func() (io.ReadCloser, error) {
			return os.Open(filePath)
		},
		Size: stat.Size(),
	}
	f.Close()

	opts = append(opts, Files(file))
	return c.Post(ctx, urlPath, opts...)
}

// ==================== Testing Helpers ====================

// MockResponse represents a mock HTTP response for testing
type MockResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Error      error
}

// MockTransport implements http.RoundTripper for testing
type MockTransport struct {
	mu        sync.Mutex
	responses map[string]MockResponse // key: "METHOD URL"
	defaultFn func(*http.Request) (*http.Response, error)
	calls     []MockCall
}

// MockCall records a request made to MockTransport
type MockCall struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// NewMockTransport creates a new mock transport
func NewMockTransport() *MockTransport {
	return &MockTransport{
		responses: make(map[string]MockResponse),
	}
}

// On registers a mock response for a method and URL pattern
func (m *MockTransport) On(method, urlPattern string, resp MockResponse) *MockTransport {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses[method+" "+urlPattern] = resp
	return m
}

// OnAny registers a default handler for unmatched requests
func (m *MockTransport) OnAny(fn func(*http.Request) (*http.Response, error)) *MockTransport {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultFn = fn
	return m
}

// Calls returns all recorded calls
func (m *MockTransport) Calls() []MockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]MockCall{}, m.calls...)
}

// Reset clears all recorded calls
func (m *MockTransport) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = m.calls[:0]
}

// RoundTrip implements http.RoundTripper
func (m *MockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	m.mu.Lock()

	// Record call
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, _ = io.ReadAll(req.Body)
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
	m.calls = append(m.calls, MockCall{
		Method: req.Method,
		URL:    req.URL.String(),
		Header: req.Header.Clone(),
		Body:   bodyBytes,
	})

	key := req.Method + " " + req.URL.String()
	resp, ok := m.responses[key]
	if !ok {
		// Try without query string
		key = req.Method + " " + req.URL.Path
		resp, ok = m.responses[key]
	}
	defaultFn := m.defaultFn
	m.mu.Unlock()

	if !ok {
		if defaultFn != nil {
			return defaultFn(req)
		}
		return nil, fmt.Errorf("no mock response for %s %s", req.Method, req.URL)
	}

	if resp.Error != nil {
		return nil, resp.Error
	}

	headers := resp.Headers
	if headers == nil {
		headers = make(http.Header)
	}

	return &http.Response{
		StatusCode: resp.StatusCode,
		Status:     http.StatusText(resp.StatusCode),
		Header:     headers,
		Body:       io.NopCloser(bytes.NewReader(resp.Body)),
		Request:    req,
	}, nil
}

// RecordTransport wraps a transport and records all requests/responses
type RecordTransport struct {
	Transport http.RoundTripper
	mu        sync.Mutex
	records   []Record
}

// Record represents a recorded request/response pair
type Record struct {
	Request   *http.Request
	Response  *http.Response
	ReqBody   []byte
	RespBody  []byte
	Error     error
	Duration  time.Duration
	StartTime time.Time
}

// NewRecordTransport creates a recording transport wrapper
func NewRecordTransport(transport http.RoundTripper) *RecordTransport {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &RecordTransport{Transport: transport}
}

// RoundTrip implements http.RoundTripper
func (rt *RecordTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	// Read request body
	var reqBody []byte
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}

	resp, err := rt.Transport.RoundTrip(req)

	record := Record{
		Request:   req,
		ReqBody:   reqBody,
		Error:     err,
		Duration:  time.Since(start),
		StartTime: start,
	}

	if resp != nil {
		// Read and restore response body
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		record.Response = resp
		record.RespBody = respBody
	}

	rt.mu.Lock()
	rt.records = append(rt.records, record)
	rt.mu.Unlock()

	return resp, err
}

// Records returns all recorded request/response pairs
func (rt *RecordTransport) Records() []Record {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]Record{}, rt.records...)
}

// Reset clears all records
func (rt *RecordTransport) Reset() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.records = rt.records[:0]
}

// ==================== Gzip Helpers ====================

// gzipBody wraps a body factory with gzip compression
func gzipBody(factory func() (io.ReadCloser, string, int64, error)) func() (io.ReadCloser, string, int64, error) {
	return func() (io.ReadCloser, string, int64, error) {
		rc, ct, _, err := factory()
		if err != nil {
			return nil, "", -1, err
		}

		pr, pw := io.Pipe()

		go func() {
			defer rc.Close()

			gz := gzip.NewWriter(pw)
			// ปิดฝั่ง writer ให้ตรงตาม error
			var copyErr error
			if _, err := io.Copy(gz, rc); err != nil {
				copyErr = err
			}
			if err := gz.Close(); err != nil && copyErr == nil {
				copyErr = err
			}

			if copyErr != nil {
				_ = pw.CloseWithError(copyErr)
				return
			}
			_ = pw.Close()
		}()

		return pr, ct, -1, nil
	}
}

type gzipReadCloser struct {
	*gzip.Reader
	src io.Closer
}

func (g *gzipReadCloser) Close() error {
	err1 := g.Reader.Close()
	err2 := g.src.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// decompressReader wraps a reader to decompress gzip
func decompressReader(r io.Reader) (io.ReadCloser, error) {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	if rc, ok := r.(io.ReadCloser); ok {
		return &gzipReadCloser{Reader: gr, src: rc}, nil
	}
	return gr, nil
}
