package main

import (
	"net/http"
	"strings"
	"testing"
)

// Every origin-denied 403 must advertise Vary: Origin so shared caches never
// replay a rejection for a later, allowed origin.
func TestOriginDeniedSetsVary(t *testing.T) {
	_, srv, token := testApp(t)
	send := func(build func(*http.Request)) *http.Response {
		t.Helper()
		req, err := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "127.0.0.1:3100"
		req.Header.Set("Authorization", "Bearer "+token)
		build(req)
		response, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { response.Body.Close() })
		return response
	}

	single := send(func(r *http.Request) { r.Header.Set("Origin", "https://evil.invalid") })
	if single.StatusCode != http.StatusForbidden {
		t.Fatalf("denied origin status = %d, want 403", single.StatusCode)
	}
	if vary := single.Header.Values("Vary"); len(vary) == 0 || !strings.Contains(strings.Join(vary, ","), "Origin") {
		t.Fatalf("single-origin 403 Vary = %v, want Origin", vary)
	}

	repeated := send(func(r *http.Request) {
		r.Header.Add("Origin", "https://one.invalid")
		r.Header.Add("Origin", "https://two.invalid")
	})
	if repeated.StatusCode != http.StatusForbidden {
		t.Fatalf("repeated origin status = %d, want 403", repeated.StatusCode)
	}
	if vary := repeated.Header.Values("Vary"); len(vary) == 0 || !strings.Contains(strings.Join(vary, ","), "Origin") {
		t.Fatalf("repeated-origin 403 Vary = %v, want Origin", vary)
	}
}

// CORS preflights reject unknown origins before authentication; that 403 also
// needs Vary: Origin.
func TestCORSPreflightDenialSetsVary(t *testing.T) {
	_, srv, _ := v113OriginServer(t, func(cfg *Config) {
		cfg.AllowedOrigins = []string{"https://app.example"}
	})
	req, err := http.NewRequest("OPTIONS", srv.URL+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://evil.invalid")
	req.Header.Set("Access-Control-Request-Method", "POST")
	response, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("preflight status = %d, want 403", response.StatusCode)
	}
	if vary := response.Header.Values("Vary"); len(vary) == 0 || !strings.Contains(strings.Join(vary, ","), "Origin") {
		t.Fatalf("preflight 403 Vary = %v, want Origin", vary)
	}
}
