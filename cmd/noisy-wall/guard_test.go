package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h := guard("127.0.0.1:8099", ok)

	cases := []struct {
		name, method, host, origin, site string
		want                             int
	}{
		{"own page reads", "GET", "localhost:8099", "", "", 200},
		{"own page presses", "POST", "localhost:8099", "http://localhost:8099", "same-origin", 200},
		{"curl presses", "POST", "127.0.0.1:8099", "", "", 200},
		{"other site presses", "POST", "localhost:8099", "https://evil.example", "cross-site", 403},
		{"other site, no origin", "POST", "localhost:8099", "", "cross-site", 403},
		{"dns rebinding reads", "GET", "evil.example:8099", "", "", 403},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/api/button", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
	}
}

func TestGuardAllowsAnyHostWhenListeningWide(t *testing.T) {
	h := guard(":8099", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "wall.lan:8099"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("got %d, want 200", w.Code)
	}
}
