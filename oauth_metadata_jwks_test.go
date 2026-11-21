package main

import (
	"net/http"
	"strings"
	"testing"
)

// This server serves no JWKS document, so the metadata endpoint must omit the
// jwks_uri member instead of advertising an empty URL. Every other field stays
// exactly as before.
func TestAuthServerMetadataOmitsEmptyJWKSURI(t *testing.T) {
	_, srv, _ := v113OriginServer(t, func(cfg *Config) {
		cfg.OAuth = &OAuthConfig{Enabled: true}
	})
	client := oauthNoRedirectClient(srv)
	response := oauthSend(t, client, http.MethodGet, srv.URL+"/.well-known/oauth-authorization-server", "", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("metadata status = %d, want 200", response.StatusCode)
	}
	body := oauthBody(t, response)
	if strings.Contains(body, "jwks_uri") {
		t.Fatalf("metadata advertises an empty JWKS URI: %s", body)
	}
	base := srv.URL
	for _, want := range []string{
		`"issuer":"` + base + `"`,
		`"authorization_endpoint":"` + base + `/oauth/authorize"`,
		`"token_endpoint":"` + base + `/oauth/token"`,
		`"registration_endpoint":"` + base + `/oauth/register"`,
		`"revocation_endpoint":"` + base + `/oauth/revoke"`,
		`"code_challenge_methods_supported":["S256"]`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metadata lost %s: %s", want, body)
		}
	}
}
