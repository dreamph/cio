package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/dreamph/cio"
)

type User struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	ctx := context.Background()

	// ============================================
	// BASIC CLIENT WITH INTERCEPTORS
	// ============================================
	c := cio.New(
		cio.BaseURL("http://localhost:3000"),
		cio.OnRequest(func(req *http.Request) {
			log.Printf("-> %s %s", req.Method, req.URL)
		}),
		cio.OnResponse(func(resp *http.Response) {
			log.Printf("<- %d %s", resp.StatusCode, resp.Request.URL)
		}),
	)

	// GET - buffered
	resp, _ := c.Get(ctx, "/users")
	users, _ := cio.Json[[]User](resp)
	fmt.Println("Users:", users)

	// ============================================
	// COOKIE JAR
	// ============================================
	c2 := cio.New(
		cio.BaseURL("http://localhost:3000"),
		cio.WithCookieJar(), // enable cookie management
	)

	// Login (server sets cookies)
	c2.Post(ctx, "/login", cio.Body(map[string]string{
		"username": "john",
		"password": "secret",
	}))

	// Subsequent requests include cookies automatically
	resp, _ = c2.Get(ctx, "/profile")
	fmt.Println("Profile:", resp.String())

	// Get cookies
	cookies, _ := c2.Cookies("http://localhost:3000")
	for _, cookie := range cookies {
		fmt.Printf("Cookie: %s=%s\n", cookie.Name, cookie.Value)
	}

	// Set cookies manually
	c2.SetCookies("http://localhost:3000", []*http.Cookie{
		{Name: "session", Value: "abc123"},
	})

	// ============================================
	// REDIRECT CONTROL
	// ============================================

	// Limit redirects
	c3 := cio.New(
		cio.BaseURL("http://localhost:3000"),
		cio.WithRedirects(3), // max 3 redirects
	)
	resp, _ = c3.Get(ctx, "/redirect-chain")

	// Disable redirects entirely
	c4 := cio.New(
		cio.BaseURL("http://localhost:3000"),
		cio.NoRedirects(),
	)
	resp, _ = c4.Get(ctx, "/redirect")
	fmt.Println("Redirect location:", resp.Headers.Get("Location"))

	// ============================================
	// REQUEST ID / TRACING
	// ============================================

	// Auto request ID
	c5 := cio.New(
		cio.BaseURL("http://localhost:3000"),
		cio.WithRequestID(randomID), // adds X-Request-ID header
	)
	c5.Get(ctx, "/api")

	// Full tracing setup
	c6 := cio.New(
		cio.BaseURL("http://localhost:3000"),
		cio.WithTracing("my-service"), // X-Request-ID + X-Service-Name
	)
	c6.Get(ctx, "/api")

	// ============================================
	// RETRY
	// ============================================

	// Retry 3 times with 100ms backoff (default: retry on any error)
	resp, err := c.Get(ctx, "/flaky-endpoint", cio.Retry(3, 100))
	if err != nil {
		fmt.Println("Failed after retries:", err)
	}

	// Retry on 5xx only
	resp, err = c.Get(ctx, "/api", cio.Retry(3, 100, cio.When5xx()))

	// Retry on specific status codes
	resp, err = c.Get(ctx, "/api", cio.Retry(3, 100, cio.WhenStatus(500, 502, 503, 504)))

	// Retry with custom condition
	resp, err = c.Get(ctx, "/api", cio.Retry(3, 100, cio.When(func(resp *cio.Response, err error) bool {
		if err != nil {
			return true
		}
		return resp.StatusCode == 429 || resp.StatusCode >= 500
	})))

	// ============================================
	// EXPECT STATUS
	// ============================================

	// Expect 2xx status
	resp, err = c.Get(ctx, "/users", cio.ExpectOK())
	if err != nil {
		var statusErr *cio.StatusError
		if errors.As(err, &statusErr) {
			fmt.Printf("Status error: %d - %s\n", statusErr.StatusCode, statusErr.Body)
		}
	}

	// ============================================
	// STREAMING
	// ============================================

	// Stream to file
	file, _ := os.Create("downloaded.bin")
	resp, _ = c.Get(ctx, "/large-file", cio.OutputStream(file))
	file.Close()
	fmt.Printf("Downloaded %d bytes\n", resp.Written)

	// ============================================
	// MULTIPART UPLOAD
	// ============================================

	f1, _ := os.Open("photo.jpg")
	resp, _ = c.Post(ctx, "/upload",
		cio.Files(cio.NewFile("file", "photo.jpg", f1)),
		cio.FormFields(map[string]string{"title": "My Photo"}),
	)
	f1.Close()
	fmt.Println("Upload:", resp.String())

	// Upload from reader
	data := strings.NewReader("hello world")
	resp, _ = c.Post(ctx, "/upload",
		cio.Files(cio.NewFile("file", "hello.txt", data)),
		cio.ExpectOK(),
	)
	fmt.Println("Upload:", resp.String())
}
