// Package fetch is the one place the exporter talks to the network. Every
// upstream (ANEEL's open-data API, the Celesc tariff page) goes through Get,
// which enforces HTTPS, a timeout, a same-host redirect policy and a cap on the
// response size, so a misbehaving or hostile upstream can't hang the exporter
// or make it buffer an unbounded body.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// MaxBodyBytes caps how much of a response body is read. The largest upstream
// (the Celesc page) is under 100 KiB, so 2 MiB is generous.
const MaxBodyBytes = 2 << 20

// UserAgent identifies the exporter to upstreams.
const UserAgent = "energia-tarifa-exporter (+https://github.com/MateuxLucax/energia-tarifa-exporter)"

// ErrTooLarge is returned when a body exceeds MaxBodyBytes.
var ErrTooLarge = errors.New("response body too large")

// NewClient returns an HTTP client with a request timeout and a redirect
// policy that only follows HTTPS redirects on the same host.
func NewClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("refusing redirect to %s", req.URL.Redacted())
			}
			return nil
		},
	}
}

// Get fetches u and returns its body. Non-2xx statuses are errors, and so is a
// body larger than MaxBodyBytes. Plain HTTP is only accepted when allowHTTP is
// set, which exists for tests against httptest servers.
func Get(ctx context.Context, c *http.Client, u *url.URL, allowHTTP bool) ([]byte, error) {
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return nil, fmt.Errorf("refusing non-https URL %s", u.Redacted())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("GET %s: status %d", u.Redacted(), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxBodyBytes {
		return nil, ErrTooLarge
	}
	return body, nil
}
