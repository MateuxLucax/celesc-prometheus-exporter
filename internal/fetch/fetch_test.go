package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseBR(t *testing.T) {
	for in, want := range map[string]float64{"345,71": 345.71, "1.234,56": 1234.56, ",00": 0, "0,27%": 0.27, " 18,85 ": 18.85} {
		got, err := ParseBR(in)
		if err != nil || got != want {
			t.Errorf("ParseBR(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "NaN", "Inf", "1,2,3"} {
		if _, err := ParseBR(in); err == nil {
			t.Errorf("ParseBR(%q): want error", in)
		}
	}
}

func TestGetLimitsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(strings.Repeat("x", MaxBodyBytes+10)))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	if _, err := Get(context.Background(), srv.Client(), u, true); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestGetRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	if _, err := Get(context.Background(), srv.Client(), u, true); err == nil {
		t.Error("5xx: want error")
	}
	if _, err := Get(context.Background(), srv.Client(), u, false); err == nil {
		t.Error("http without allowHTTP: want error")
	}
}

func TestRedirectPolicy(t *testing.T) {
	check := NewClient().CheckRedirect
	from, _ := http.NewRequest("GET", "https://www.celesc.com.br/a", nil)
	for target, ok := range map[string]bool{
		"https://www.celesc.com.br/b": true,
		"http://www.celesc.com.br/b":  false,
		"https://evil.example/b":      false,
	} {
		to, _ := http.NewRequest("GET", target, nil)
		if err := check(to, []*http.Request{from}); (err == nil) != ok {
			t.Errorf("redirect to %s: err=%v, want allowed=%v", target, err, ok)
		}
	}
}
