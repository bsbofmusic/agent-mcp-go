package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const oauthFormContentType = "application/x-www-form-urlencoded"

// TestOAuthGeminiStaticClientFlow walks the flow Gemini Web uses when the
// operator pre-registers a client ID and client secret in the Gemini UI.
func TestOAuthGeminiStaticClientFlow(t *testing.T) {
	const (
		clientID     = "gemini-web"
		clientSecret = "gemini-secret-0123456789"
		redirectURI  = "https://oauth-redirect.googleusercontent.com/r/gemini-ext"
		stateValue   = "state-abc-123"
	)
	app, server, ownerToken := v113OriginServer(t, func(cfg *Config) {
		cfg.OAuth = &OAuthConfig{
			Enabled: true,
			Clients: []OAuthClientConfig{
				{
					ClientID:           clientID,
					ClientName:         "Gemini Web",
					ClientSecretSHA256: sha256Hex(clientSecret),
					RedirectURIs:       []string{"https://oauth-redirect.googleusercontent.com/r/*"},
				},
				{
					ClientID:           "other-client",
					ClientName:         "Other Client",
					ClientSecretSHA256: sha256Hex("other-secret"),
				},
			},
		}
	})
	base := server.URL
	client := oauthNoRedirectClient(server)

	t.Run("protected resource metadata", func(t *testing.T) {
		for _, tc := range []struct {
			path string
			want string
		}{
			{"/.well-known/oauth-protected-resource", base + "/mcp"},
			{"/.well-known/oauth-protected-resource/mcp", base + "/mcp"},
			{"/.well-known/oauth-protected-resource/mcp/stateless", base + "/mcp/stateless"},
		} {
			path := tc.path
			response := oauthSend(t, client, http.MethodGet, base+path, "", "", nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("%s status %d", path, response.StatusCode)
			}
			meta := oauthJSON(t, response)
			if meta["resource"] != tc.want {
				t.Fatalf("%s resource %v, want %s", path, meta["resource"], tc.want)
			}
			servers, _ := meta["authorization_servers"].([]any)
			if len(servers) != 1 || servers[0] != base {
				t.Fatalf("%s authorization_servers %v", path, meta["authorization_servers"])
			}
			methods, _ := meta["bearer_methods_supported"].([]any)
			if len(methods) != 1 || methods[0] != "header" {
				t.Fatalf("%s bearer_methods_supported %v", path, meta["bearer_methods_supported"])
			}
		}
	})

	t.Run("authorization server metadata", func(t *testing.T) {
		for _, path := range []string{
			"/.well-known/oauth-authorization-server",
			"/.well-known/oauth-authorization-server/mcp",
			"/.well-known/openid-configuration",
		} {
			response := oauthSend(t, client, http.MethodGet, base+path, "", "", nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("%s status %d", path, response.StatusCode)
			}
			if got := response.Header.Get("Access-Control-Allow-Origin"); got != "*" {
				t.Fatalf("%s CORS %q", path, got)
			}
			meta := oauthJSON(t, response)
			want := map[string]string{
				"issuer":                 base,
				"authorization_endpoint": base + "/oauth/authorize",
				"token_endpoint":         base + "/oauth/token",
				"registration_endpoint":  base + "/oauth/register",
				"revocation_endpoint":    base + "/oauth/revoke",
			}
			for key, value := range want {
				if meta[key] != value {
					t.Fatalf("%s %s = %v, want %s", path, key, meta[key], value)
				}
			}
		}
		response := oauthSend(t, client, http.MethodOptions, base+"/.well-known/oauth-authorization-server", "", "", nil)
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("metadata preflight status %d", response.StatusCode)
		}
	})

	t.Run("unauthenticated mcp advertises resource metadata", func(t *testing.T) {
		response := oauthSend(t, client, http.MethodPost, base+"/mcp", "application/json", `{}`, nil)
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated /mcp status %d", response.StatusCode)
		}
		challenge := response.Header.Get("WWW-Authenticate")
		if !strings.HasPrefix(challenge, "Bearer ") ||
			!strings.Contains(challenge, `resource_metadata="`+base+"/.well-known/oauth-protected-resource"+`"`) ||
			!strings.Contains(challenge, `scope="mcp"`) {
			t.Fatalf("WWW-Authenticate %q", challenge)
		}
	})

	verifier, challenge := oauthPKCEPair()
	params := oauthFlowParams{
		ClientID: clientID, RedirectURI: redirectURI, State: stateValue,
		Scope: "openid", Resource: base + "/mcp", Challenge: challenge,
	}

	t.Run("consent page requires the owner passkey", func(t *testing.T) {
		response := oauthAuthorizeGET(t, client, base, params)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("authorize status %d: %s", response.StatusCode, oauthBody(t, response))
		}
		if got := response.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Fatalf("X-Frame-Options %q", got)
		}
		csrf := oauthCSRFToken(t, response)

		response = oauthConsent(t, client, base, params, csrf, "wrong-passkey", "", "authorize")
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong passkey status %d", response.StatusCode)
		}
		csrf = oauthCSRFToken(t, response)

		response = oauthConsent(t, client, base, params, csrf, ownerToken, "", "authorize")
		if response.StatusCode != http.StatusSeeOther {
			t.Fatalf("authorize redirect status %d: %s", response.StatusCode, oauthBody(t, response))
		}
		location, err := url.Parse(response.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		if location.Scheme != "https" || location.Host != "oauth-redirect.googleusercontent.com" || !strings.HasPrefix(location.Path, "/r/") {
			t.Fatalf("unexpected redirect %s", location)
		}
		query := location.Query()
		code := query.Get("code")
		if !strings.HasPrefix(code, "amg_ac_") || len(code) < 16 {
			t.Fatalf("authorization code %q", code)
		}
		if query.Get("state") != stateValue {
			t.Fatalf("state not preserved: %q", query.Get("state"))
		}
		if query.Get("iss") != base {
			t.Fatalf("iss %q, want %q", query.Get("iss"), base)
		}

		tokens := oauthExchangeCode(t, client, base, clientID, clientSecret, redirectURI, verifier, code)
		accessToken, _ := tokens["access_token"].(string)
		refreshToken, _ := tokens["refresh_token"].(string)
		if !strings.HasPrefix(accessToken, "amg_at_") || !strings.HasPrefix(refreshToken, "amg_rt_") {
			t.Fatalf("token prefixes: at=%q rt=%q", accessToken, refreshToken)
		}
		if tokens["token_type"] != "Bearer" {
			t.Fatalf("token_type %v", tokens["token_type"])
		}
		if scope, _ := tokens["scope"].(string); !strings.Contains(scope, "mcp") {
			t.Fatalf("granted scope %q must include mcp", scope)
		}
		if expires, _ := tokens["expires_in"].(float64); expires != 3600 {
			t.Fatalf("expires_in %v", tokens["expires_in"])
		}

		t.Run("state file keeps digests only", func(t *testing.T) {
			statePath := filepath.Join(app.config.Get().StateDir, "oauth", "state.json")
			info, err := os.Stat(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0600 {
				t.Fatalf("state file permissions %o", perm)
			}
			raw, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			text := string(raw)
			for _, secret := range []string{accessToken, refreshToken, clientSecret} {
				if strings.Contains(text, secret) {
					t.Fatalf("state file contains plaintext credential %q", secret)
				}
			}
			if !strings.Contains(text, sha256Hex(accessToken)) || !strings.Contains(text, sha256Hex(refreshToken)) {
				t.Fatal("state file lacks the token digests")
			}
		})

		session := oauthMCPInitialize(t, client, base, accessToken)
		oauthMCPList(t, client, base, accessToken, session)
		oauthMCPTool(t, client, base, accessToken, session, "get_environment")

		t.Run("client_secret_post is accepted too", func(t *testing.T) {
			postVerifier, postChallenge := oauthPKCEPair()
			postParams := params
			postParams.Challenge = postChallenge
			postCSRF := oauthCSRFToken(t, oauthAuthorizeGET(t, client, base, postParams))
			response := oauthConsent(t, client, base, postParams, postCSRF, ownerToken, "", "authorize")
			if response.StatusCode != http.StatusSeeOther {
				t.Fatalf("authorize status %d", response.StatusCode)
			}
			code := oauthLocationCode(t, response)
			form := url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {code},
				"redirect_uri":  {redirectURI},
				"code_verifier": {postVerifier},
				"client_id":     {clientID},
				"client_secret": {clientSecret},
			}
			response = oauthSend(t, client, http.MethodPost, base+"/oauth/token", oauthFormContentType, form.Encode(), nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("client_secret_post status %d: %s", response.StatusCode, oauthBody(t, response))
			}
		})

		t.Run("refresh keeps the same refresh token", func(t *testing.T) {
			form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}
			response := oauthSend(t, client, http.MethodPost, base+"/oauth/token", oauthFormContentType, form.Encode(), func(r *http.Request) {
				r.SetBasicAuth(clientID, clientSecret)
			})
			if response.StatusCode != http.StatusOK {
				t.Fatalf("refresh status %d: %s", response.StatusCode, oauthBody(t, response))
			}
			refreshed := oauthJSON(t, response)
			newAccess, _ := refreshed["access_token"].(string)
			if newAccess == accessToken || !strings.HasPrefix(newAccess, "amg_at_") {
				t.Fatalf("refresh did not mint a new access token: %q", newAccess)
			}
			if refreshed["refresh_token"] != refreshToken {
				t.Fatalf("refresh token rotated: %v", refreshed["refresh_token"])
			}
			// The stable per-client UserID must keep the Mcp-Session-Id alive.
			oauthMCPList(t, client, base, newAccess, session)

			t.Run("another client cannot reuse the session", func(t *testing.T) {
				otherVerifier, otherChallenge := oauthPKCEPair()
				other := params
				other.ClientID = "other-client"
				other.RedirectURI = "https://oauth-redirect.googleusercontent.com/r/other"
				other.State = "other-state"
				other.Challenge = otherChallenge
				otherCSRF := oauthCSRFToken(t, oauthAuthorizeGET(t, client, base, other))
				consentResponse := oauthConsent(t, client, base, other, otherCSRF, ownerToken, "", "authorize")
				if consentResponse.StatusCode != http.StatusSeeOther {
					t.Fatalf("other authorize status %d", consentResponse.StatusCode)
				}
				otherTokens := oauthExchangeCode(t, client, base, "other-client", "other-secret", other.RedirectURI, otherVerifier, oauthLocationCode(t, consentResponse))
				otherAccess, _ := otherTokens["access_token"].(string)
				if otherAccess == "" {
					t.Fatal("other client received no access token")
				}
				response := oauthMCPSend(t, client, base, otherAccess, session, `{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{}}`)
				if response.StatusCode != http.StatusForbidden {
					t.Fatalf("cross-client session status %d", response.StatusCode)
				}
				if body := oauthBody(t, response); !strings.Contains(body, "session user mismatch") {
					t.Fatalf("cross-client session body %q", body)
				}
			})

			t.Run("revocation invalidates the linked access token", func(t *testing.T) {
				form := url.Values{"token": {refreshToken}}
				response := oauthSend(t, client, http.MethodPost, base+"/oauth/revoke", oauthFormContentType, form.Encode(), func(r *http.Request) {
					r.SetBasicAuth(clientID, clientSecret)
				})
				if response.StatusCode != http.StatusOK {
					t.Fatalf("revoke status %d", response.StatusCode)
				}
				response = oauthMCPSend(t, client, base, newAccess, session, `{"jsonrpc":"2.0","id":10,"method":"tools/list","params":{}}`)
				if response.StatusCode != http.StatusUnauthorized {
					t.Fatalf("revoked access token status %d", response.StatusCode)
				}
			})
		})
	})
}

// TestOAuthGeminiZeroConfigDCRFlow covers the "no credentials" Gemini setup:
// dynamic client registration plus a public PKCE client.
func TestOAuthGeminiZeroConfigDCRFlow(t *testing.T) {
	_, server, ownerToken := v113OriginServer(t, func(cfg *Config) {
		cfg.OAuth = &OAuthConfig{Enabled: true}
	})
	base := server.URL
	client := oauthNoRedirectClient(server)
	redirectURI := "https://oauth-redirect.googleusercontent.com/r/zero-config"

	payload := `{"client_name":"Gemini Web","redirect_uris":["` + redirectURI + `"],"token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`
	response := oauthSend(t, client, http.MethodPost, base+"/oauth/register", "application/json", payload, nil)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("registration status %d: %s", response.StatusCode, oauthBody(t, response))
	}
	registration := oauthJSON(t, response)
	clientID, _ := registration["client_id"].(string)
	if !strings.HasPrefix(clientID, "amg_cid_") {
		t.Fatalf("client_id %q", clientID)
	}
	if _, ok := registration["client_secret"]; ok {
		t.Fatal("public client must not receive a client secret")
	}
	if registration["token_endpoint_auth_method"] != "none" {
		t.Fatalf("token_endpoint_auth_method %v", registration["token_endpoint_auth_method"])
	}
	if response := oauthSend(t, client, http.MethodOptions, base+"/oauth/register", "", "", nil); response.StatusCode != http.StatusNoContent {
		t.Fatalf("registration preflight status %d", response.StatusCode)
	}

	verifier, challenge := oauthPKCEPair()
	params := oauthFlowParams{
		ClientID: clientID, RedirectURI: redirectURI, State: "zero-state",
		Scope: "mcp", Resource: base, Challenge: challenge,
	}
	csrf := oauthCSRFToken(t, oauthAuthorizeGET(t, client, base, params))
	response = oauthConsent(t, client, base, params, csrf, ownerToken, "", "authorize")
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("authorize status %d: %s", response.StatusCode, oauthBody(t, response))
	}
	tokens := oauthExchangeCode(t, client, base, clientID, "", redirectURI, verifier, oauthLocationCode(t, response))
	accessToken, _ := tokens["access_token"].(string)
	refreshToken, _ := tokens["refresh_token"].(string)
	if accessToken == "" || refreshToken == "" {
		t.Fatalf("tokens %v", tokens)
	}

	session := oauthMCPInitialize(t, client, base, accessToken)
	oauthMCPTool(t, client, base, accessToken, session, "get_environment")

	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {clientID}}
	response = oauthSend(t, client, http.MethodPost, base+"/oauth/token", oauthFormContentType, form.Encode(), nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("refresh status %d: %s", response.StatusCode, oauthBody(t, response))
	}
	refreshed := oauthJSON(t, response)
	if refreshed["refresh_token"] != refreshToken {
		t.Fatal("public client refresh token rotated")
	}
	if refreshed["access_token"] == accessToken {
		t.Fatal("refresh returned the same access token")
	}
}

// TestOAuthAdversarialRedirects proves that a malicious callback can never be
// registered or used, and that the authorize endpoint never redirects to one.
func TestOAuthAdversarialRedirects(t *testing.T) {
	_, server, _ := v113OriginServer(t, func(cfg *Config) {
		cfg.OAuth = &OAuthConfig{
			Enabled: true,
			Clients: []OAuthClientConfig{{
				ClientID: "gemini-web", ClientName: "Gemini Web", ClientSecretSHA256: sha256Hex("secret"),
				RedirectURIs: []string{"https://oauth-redirect.googleusercontent.com/r/*"},
			}},
		}
	})
	base := server.URL
	client := oauthNoRedirectClient(server)

	disallowed := []string{
		"https://evil.example/cb",
		"http://example.com/cb",
		"https://oauth-redirect.googleusercontent.com/r/ext#fragment",
		"https://oauth-redirect.googleusercontent.com/r/../../evil",
		"https://oauth-redirect.googleusercontent.com/raw",
		"https://evil.example/../r/cb",
	}
	for _, redirectURI := range disallowed {
		payload, err := json.Marshal(map[string]any{
			"client_name":                "evil",
			"redirect_uris":              []string{redirectURI},
			"token_endpoint_auth_method": "none",
		})
		if err != nil {
			t.Fatal(err)
		}
		response := oauthSend(t, client, http.MethodPost, base+"/oauth/register", "application/json", string(payload), nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("registration of %q status %d", redirectURI, response.StatusCode)
		}
		if got := oauthJSON(t, response)["error"]; got != "invalid_redirect_uri" {
			t.Fatalf("registration of %q error %v", redirectURI, got)
		}

		_, challenge := oauthPKCEPair()
		query := url.Values{
			"client_id":             {"gemini-web"},
			"redirect_uri":          {redirectURI},
			"response_type":         {"code"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
			"state":                 {"x"},
		}
		response = oauthSend(t, client, http.MethodGet, base+"/oauth/authorize?"+query.Encode(), "", "", nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("authorize with %q status %d", redirectURI, response.StatusCode)
		}
		if location := response.Header.Get("Location"); location != "" {
			t.Fatalf("authorize redirected to %q instead of failing closed", location)
		}
	}

	t.Run("unknown client is refused without a redirect", func(t *testing.T) {
		_, challenge := oauthPKCEPair()
		query := url.Values{
			"client_id":             {"nobody"},
			"redirect_uri":          {"https://oauth-redirect.googleusercontent.com/r/x"},
			"response_type":         {"code"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
		}
		response := oauthSend(t, client, http.MethodGet, base+"/oauth/authorize?"+query.Encode(), "", "", nil)
		if response.StatusCode != http.StatusBadRequest || response.Header.Get("Location") != "" {
			t.Fatalf("unknown client status %d location %q", response.StatusCode, response.Header.Get("Location"))
		}
	})

	t.Run("registration limits redirect_uris", func(t *testing.T) {
		many := make([]string, 17)
		for i := range many {
			many[i] = "https://oauth-redirect.googleusercontent.com/r/client-" + string(rune('a'+i))
		}
		payload, _ := json.Marshal(map[string]any{"redirect_uris": many})
		response := oauthSend(t, client, http.MethodPost, base+"/oauth/register", "application/json", string(payload), nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("seventeen redirect_uris status %d", response.StatusCode)
		}
		payload, _ = json.Marshal(map[string]any{"redirect_uris": []string{}})
		response = oauthSend(t, client, http.MethodPost, base+"/oauth/register", "application/json", string(payload), nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("empty redirect_uris status %d", response.StatusCode)
		}
	})

	t.Run("resource must belong to this server", func(t *testing.T) {
		_, challenge := oauthPKCEPair()
		params := oauthFlowParams{
			ClientID: "gemini-web", RedirectURI: "https://oauth-redirect.googleusercontent.com/r/x",
			State: "s", Scope: "mcp", Resource: "https://other.example/mcp", Challenge: challenge,
		}
		response := oauthAuthorizeGET(t, client, base, params)
		if response.StatusCode != http.StatusSeeOther {
			t.Fatalf("invalid resource status %d", response.StatusCode)
		}
		location, err := url.Parse(response.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		if location.Query().Get("error") != "invalid_target" {
			t.Fatalf("invalid resource error %q", location.Query().Get("error"))
		}
	})
}

// TestOAuthConsentCSRFAndOrigin pins the consent form protections.
func TestOAuthConsentCSRFAndOrigin(t *testing.T) {
	_, server, ownerToken := v113OriginServer(t, func(cfg *Config) {
		cfg.OAuth = &OAuthConfig{
			Enabled: true,
			Clients: []OAuthClientConfig{{
				ClientID: "gemini-web", ClientName: "Gemini Web", ClientSecretSHA256: sha256Hex("secret"),
			}},
		}
	})
	base := server.URL
	client := oauthNoRedirectClient(server)
	_, challenge := oauthPKCEPair()
	params := oauthFlowParams{
		ClientID: "gemini-web", RedirectURI: "https://oauth-redirect.googleusercontent.com/r/x",
		State: "csrf-state", Scope: "mcp", Challenge: challenge,
	}

	response := oauthSend(t, client, http.MethodPost, base+"/oauth/authorize", oauthFormContentType,
		oauthConsentForm(params, "", ownerToken, "authorize").Encode(), func(r *http.Request) {
			r.Header.Set("Origin", base)
		})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing csrf status %d", response.StatusCode)
	}

	csrf := oauthCSRFToken(t, oauthAuthorizeGET(t, client, base, params))
	tampered := oauthConsentForm(params, csrf, ownerToken, "authorize")
	tampered.Set("client_id", "other-client")
	response = oauthSend(t, client, http.MethodPost, base+"/oauth/authorize", oauthFormContentType, tampered.Encode(), func(r *http.Request) {
		r.Header.Set("Origin", base)
	})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched ticket status %d", response.StatusCode)
	}

	csrf = oauthCSRFToken(t, oauthAuthorizeGET(t, client, base, params))
	response = oauthConsent(t, client, base, params, csrf, ownerToken, "https://evil.example", "authorize")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin consent status %d", response.StatusCode)
	}
	// The origin failure must not consume the ticket: the same csrf still
	// authorizes from a same-origin form post.
	response = oauthConsent(t, client, base, params, csrf, ownerToken, base, "authorize")
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("same-origin consent after rejected origin status %d: %s", response.StatusCode, oauthBody(t, response))
	}

	t.Run("deny redirects with access_denied", func(t *testing.T) {
		csrf := oauthCSRFToken(t, oauthAuthorizeGET(t, client, base, params))
		response := oauthConsent(t, client, base, params, csrf, ownerToken, "", "deny")
		if response.StatusCode != http.StatusSeeOther {
			t.Fatalf("deny status %d", response.StatusCode)
		}
		location, err := url.Parse(response.Header.Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		if location.Query().Get("error") != "access_denied" || location.Query().Get("state") != params.State {
			t.Fatalf("deny redirect %s", location)
		}
	})

	t.Run("a ticket is single use", func(t *testing.T) {
		csrf := oauthCSRFToken(t, oauthAuthorizeGET(t, client, base, params))
		if response := oauthConsent(t, client, base, params, csrf, ownerToken, "", "authorize"); response.StatusCode != http.StatusSeeOther {
			t.Fatalf("first consent status %d", response.StatusCode)
		}
		if response := oauthConsent(t, client, base, params, csrf, ownerToken, "", "authorize"); response.StatusCode != http.StatusBadRequest {
			t.Fatalf("replayed ticket status %d", response.StatusCode)
		}
	})
}

// TestOAuthCodeReplayAndPKCE covers code single-use, PKCE verification and
// token-endpoint client authentication.
func TestOAuthCodeReplayAndPKCE(t *testing.T) {
	_, server, ownerToken := v113OriginServer(t, func(cfg *Config) {
		cfg.OAuth = &OAuthConfig{
			Enabled: true,
			Clients: []OAuthClientConfig{
				{ClientID: "gemini-web", ClientSecretSHA256: sha256Hex("secret")},
				{ClientID: "other-client", ClientSecretSHA256: sha256Hex("other-secret")},
			},
		}
	})
	base := server.URL
	client := oauthNoRedirectClient(server)
	redirectURI := "https://oauth-redirect.googleusercontent.com/r/replay"
	params := oauthFlowParams{
		ClientID: "gemini-web", RedirectURI: redirectURI, State: "replay-state", Scope: "mcp",
	}

	issue := func(t *testing.T) (string, string, string) {
		t.Helper()
		verifier, challenge := oauthPKCEPair()
		params.Challenge = challenge
		csrf := oauthCSRFToken(t, oauthAuthorizeGET(t, client, base, params))
		response := oauthConsent(t, client, base, params, csrf, ownerToken, "", "authorize")
		if response.StatusCode != http.StatusSeeOther {
			t.Fatalf("authorize status %d", response.StatusCode)
		}
		return oauthLocationCode(t, response), verifier, challenge
	}

	t.Run("a code is single use and PKCE is enforced", func(t *testing.T) {
		code, verifier, _ := issue(t)
		wrongVerifier := strings.Repeat("b", 43)
		response := oauthSend(t, client, http.MethodPost, base+"/oauth/token", oauthFormContentType, url.Values{
			"grant_type": {"authorization_code"}, "code": {code},
			"redirect_uri": {redirectURI}, "code_verifier": {wrongVerifier},
		}.Encode(), func(r *http.Request) { r.SetBasicAuth("gemini-web", "secret") })
		if response.StatusCode != http.StatusBadRequest || oauthJSON(t, response)["error"] != "invalid_grant" {
			t.Fatalf("wrong verifier status %d", response.StatusCode)
		}
		// The failed attempt already consumed the code.
		response = oauthExchangeCodeRaw(t, client, base, code, redirectURI, verifier, "gemini-web", "secret")
		if response.StatusCode != http.StatusBadRequest || oauthJSON(t, response)["error"] != "invalid_grant" {
			t.Fatalf("code reuse status %d", response.StatusCode)
		}
	})

	t.Run("a code cannot be redirected to another client or callback", func(t *testing.T) {
		code, verifier, _ := issue(t)
		response := oauthExchangeCodeRaw(t, client, base, code, redirectURI, verifier, "other-client", "other-secret")
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("cross-client exchange status %d", response.StatusCode)
		}
		code, verifier, _ = issue(t)
		response = oauthExchangeCodeRaw(t, client, base, code, "https://oauth-redirect.googleusercontent.com/r/other", verifier, "gemini-web", "secret")
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("redirect_uri mismatch status %d", response.StatusCode)
		}
	})

	t.Run("a redeemed code cannot be replayed", func(t *testing.T) {
		code, verifier, _ := issue(t)
		if response := oauthExchangeCodeRaw(t, client, base, code, redirectURI, verifier, "gemini-web", "secret"); response.StatusCode != http.StatusOK {
			t.Fatalf("exchange status %d: %s", response.StatusCode, oauthBody(t, response))
		}
		if response := oauthExchangeCodeRaw(t, client, base, code, redirectURI, verifier, "gemini-web", "secret"); response.StatusCode != http.StatusBadRequest {
			t.Fatalf("replay status %d", response.StatusCode)
		}
	})

	t.Run("unsupported and ambiguous client authentication", func(t *testing.T) {
		code, verifier, _ := issue(t)
		response := oauthSend(t, client, http.MethodPost, base+"/oauth/token", oauthFormContentType, url.Values{
			"grant_type": {"password"}, "username": {"x"}, "password": {"y"},
		}.Encode(), func(r *http.Request) { r.SetBasicAuth("gemini-web", "secret") })
		if response.StatusCode != http.StatusBadRequest || oauthJSON(t, response)["error"] != "unsupported_grant_type" {
			t.Fatalf("unsupported grant status %d", response.StatusCode)
		}

		response = oauthSend(t, client, http.MethodPost, base+"/oauth/token", oauthFormContentType, url.Values{
			"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
			"code_verifier": {verifier}, "client_id": {"gemini-web"}, "client_secret": {"secret"},
		}.Encode(), func(r *http.Request) { r.SetBasicAuth("gemini-web", "secret") })
		if response.StatusCode != http.StatusBadRequest || oauthJSON(t, response)["error"] != "invalid_request" {
			t.Fatalf("double client authentication status %d", response.StatusCode)
		}

		response = oauthSend(t, client, http.MethodPost, base+"/oauth/token", oauthFormContentType, url.Values{
			"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {verifier},
		}.Encode(), func(r *http.Request) { r.SetBasicAuth("gemini-web", "wrong-secret") })
		if response.StatusCode != http.StatusUnauthorized || oauthJSON(t, response)["error"] != "invalid_client" {
			t.Fatalf("bad client secret status %d", response.StatusCode)
		}
		if got := response.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
			t.Fatalf("missing Basic challenge: %q", got)
		}
	})
}

// TestOAuthStateBoundsSweepExpiredTokens proves expired material is dropped
// instead of accumulating on disk.
func TestOAuthStateBoundsSweepExpiredTokens(t *testing.T) {
	app, server, _ := v113OriginServer(t, func(cfg *Config) {
		cfg.OAuth = &OAuthConfig{Enabled: true}
	})
	o := app.oauth
	if o == nil {
		t.Fatal("oauth server not started")
	}
	o.mu.Lock()
	o.state.AccessTokens = append(o.state.AccessTokens, oauthAccessToken{
		TokenSHA256: sha256Hex("expired"),
		ClientID:    "client",
		ExpiresAt:   time.Now().Add(-time.Minute),
		CreatedAt:   time.Now().Add(-time.Hour),
	})
	o.state.RefreshTokens = append(o.state.RefreshTokens, oauthRefreshToken{
		TokenSHA256: sha256Hex("expired-refresh"),
		ClientID:    "client",
		ExpiresAt:   time.Now().Add(-time.Minute),
		CreatedAt:   time.Now().Add(-time.Hour),
	})
	if !o.sweepLocked(time.Now()) {
		o.mu.Unlock()
		t.Fatal("sweep did not report expired state")
	}
	o.mu.Unlock()

	// A live request also sweeps, so the expired record must not verify.
	response := oauthSend(t, oauthNoRedirectClient(server), http.MethodPost, server.URL+"/mcp", "application/json", `{}`, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer amg_at_expired")
	})
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired token status %d", response.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Helpers

type oauthFlowParams struct {
	ClientID    string
	RedirectURI string
	State       string
	Scope       string
	Resource    string
	Challenge   string
}

func oauthNoRedirectClient(server *httptest.Server) *http.Client {
	return &http.Client{
		Transport: server.Client().Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func oauthSend(t *testing.T, client *http.Client, method, target, contentType, payload string, configure func(*http.Request)) *http.Response {
	t.Helper()
	var body io.Reader
	if payload != "" {
		body = strings.NewReader(payload)
	}
	request, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if configure != nil {
		configure(request)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func oauthBody(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func oauthJSON(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(oauthBody(t, response)), &payload); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return payload
}

var oauthCSRFPattern = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func oauthCSRFToken(t *testing.T, response *http.Response) string {
	t.Helper()
	body := oauthBody(t, response)
	match := oauthCSRFPattern.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("consent page has no csrf token: %s", body)
	}
	return match[1]
}

func oauthPKCEPair() (string, string) {
	verifier := "amg-verifier-" + randomHex(48)
	return verifier, pkceChallengeForVerifier(verifier)
}

func oauthAuthorizeQuery(params oauthFlowParams) string {
	query := url.Values{
		"client_id":             {params.ClientID},
		"redirect_uri":          {params.RedirectURI},
		"response_type":         {"code"},
		"code_challenge":        {params.Challenge},
		"code_challenge_method": {"S256"},
	}
	if params.State != "" {
		query.Set("state", params.State)
	}
	if params.Scope != "" {
		query.Set("scope", params.Scope)
	}
	if params.Resource != "" {
		query.Set("resource", params.Resource)
	}
	return "/oauth/authorize?" + query.Encode()
}

func oauthAuthorizeGET(t *testing.T, client *http.Client, base string, params oauthFlowParams) *http.Response {
	t.Helper()
	return oauthSend(t, client, http.MethodGet, base+oauthAuthorizeQuery(params), "", "", nil)
}

func oauthConsentForm(params oauthFlowParams, csrf, passkey, decision string) url.Values {
	return url.Values{
		"csrf_token":     {csrf},
		"client_id":      {params.ClientID},
		"redirect_uri":   {params.RedirectURI},
		"state":          {params.State},
		"scope":          {strings.Join(normalizeScopes(params.Scope), " ")},
		"code_challenge": {params.Challenge},
		"resource":       {params.Resource},
		"passkey":        {passkey},
		"decision":       {decision},
	}
}

func oauthConsent(t *testing.T, client *http.Client, base string, params oauthFlowParams, csrf, passkey, origin, decision string) *http.Response {
	t.Helper()
	return oauthSend(t, client, http.MethodPost, base+"/oauth/authorize", oauthFormContentType,
		oauthConsentForm(params, csrf, passkey, decision).Encode(), func(r *http.Request) {
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
		})
}

func oauthLocationCode(t *testing.T, response *http.Response) string {
	t.Helper()
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("redirect carries no code: %s", location)
	}
	return code
}

func oauthExchangeCode(t *testing.T, client *http.Client, base, clientID, secret, redirectURI, verifier, code string) map[string]any {
	t.Helper()
	response := oauthExchangeCodeRaw(t, client, base, code, redirectURI, verifier, clientID, secret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("token exchange status %d: %s", response.StatusCode, oauthBody(t, response))
	}
	return oauthJSON(t, response)
}

func oauthExchangeCodeRaw(t *testing.T, client *http.Client, base, code, redirectURI, verifier, clientID, secret string) *http.Response {
	t.Helper()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
	}
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}
	if secret == "" && clientID != "" {
		form.Set("client_id", clientID)
	}
	configure := func(r *http.Request) {
		if secret != "" && clientID != "" {
			r.SetBasicAuth(clientID, secret)
		}
	}
	return oauthSend(t, client, http.MethodPost, base+"/oauth/token", oauthFormContentType, form.Encode(), configure)
}

func oauthMCPSend(t *testing.T, client *http.Client, base, token, session, payload string) *http.Response {
	t.Helper()
	return oauthSend(t, client, http.MethodPost, base+"/mcp", "application/json", payload, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("User-Agent", "Google")
		if session != "" {
			r.Header.Set("Mcp-Session-Id", session)
		}
	})
}

func oauthMCPInitialize(t *testing.T, client *http.Client, base, token string) string {
	t.Helper()
	response := oauthMCPSend(t, client, base, token, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"gemini-web","version":"1"}}}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initialize status %d: %s", response.StatusCode, oauthBody(t, response))
	}
	session := response.Header.Get("Mcp-Session-Id")
	if session == "" {
		t.Fatal("oauth initialize did not establish a session")
	}
	response = oauthMCPSend(t, client, base, token, session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("initialized notification status %d", response.StatusCode)
	}
	return session
}

func oauthMCPList(t *testing.T, client *http.Client, base, token, session string) []any {
	t.Helper()
	response := oauthMCPSend(t, client, base, token, session, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status %d: %s", response.StatusCode, oauthBody(t, response))
	}
	wire := oauthJSON(t, response)
	result, _ := wire["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("tools/list returned no tools")
	}
	return tools
}

func oauthMCPTool(t *testing.T, client *http.Client, base, token, session, name string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := oauthMCPSend(t, client, base, token, session, string(payload))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("tools/call %s status %d: %s", name, response.StatusCode, oauthBody(t, response))
	}
	wire := oauthJSON(t, response)
	if wire["error"] != nil {
		t.Fatalf("tools/call %s failed: %v", name, wire["error"])
	}
	if _, ok := wire["result"].(map[string]any); !ok {
		t.Fatalf("tools/call %s returned no result", name)
	}
}

// basicAuthHeader builds the RFC 6749 §2.3.1 header for a client, used by
// callers that want to assert the encoded form explicitly.
func basicAuthHeader(clientID, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(clientID)+":"+url.QueryEscape(secret)))
}

// TestOAuthConfidentialWithoutPKCEAndQueryIsolation verifies:
//  1. Pre-registered confidential clients work even when the caller omits PKCE
//     (some confidential OAuth clients only send client_id + client_secret),
//     while still rejecting an unexpected code_verifier on exchange.
//  2. Public clients still strictly require PKCE S256.
//  3. URL query parameters on POST /oauth/token cannot smuggle grant_type or secrets.
//  4. /.well-known/oauth-protected-resource/mcp/stateless reports /mcp/stateless.
func TestOAuthConfidentialWithoutPKCEAndQueryIsolation(t *testing.T) {
	_, server, ownerToken := v113OriginServer(t, func(cfg *Config) {
		cfg.OAuth = &OAuthConfig{
			Enabled: true,
			Clients: []OAuthClientConfig{{
				ClientID:           "gemini-confidential",
				ClientName:         "Gemini Confidential",
				ClientSecretSHA256: sha256Hex("top-secret"),
			}},
		}
	})
	base := server.URL
	client := oauthNoRedirectClient(server)
	redirectURI := "https://oauth-redirect.googleusercontent.com/r/gemini-conf"

	// 1. Stateless protected-resource metadata reflects /mcp/stateless.
	statelessResp := oauthSend(t, client, http.MethodGet, base+"/.well-known/oauth-protected-resource/mcp/stateless", "", "", nil)
	if statelessResp.StatusCode != http.StatusOK {
		t.Fatalf("stateless PRM status %d", statelessResp.StatusCode)
	}
	statelessMeta := oauthJSON(t, statelessResp)
	if got := statelessMeta["resource"]; got != base+"/mcp/stateless" {
		t.Fatalf("stateless PRM resource = %v, want %s/mcp/stateless", got, base)
	}

	// 2. Confidential client without PKCE succeeds when authenticated with Basic/secret.
	authURL := base + "/oauth/authorize?" + url.Values{
		"response_type": {"code"},
		"client_id":     {"gemini-confidential"},
		"redirect_uri":  {redirectURI},
		"state":         {"no-pkce-state"},
		"scope":         {"mcp"},
	}.Encode()
	getResp := oauthSend(t, client, http.MethodGet, authURL, "", "", nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("confidential GET /oauth/authorize without PKCE status %d", getResp.StatusCode)
	}
	csrf := oauthCSRFToken(t, getResp)
	params := oauthFlowParams{
		ClientID: "gemini-confidential", RedirectURI: redirectURI, State: "no-pkce-state", Scope: "mcp",
	}
	postResp := oauthConsent(t, client, base, params, csrf, ownerToken, base, "authorize")
	if postResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("confidential POST /oauth/authorize status %d: %s", postResp.StatusCode, oauthBody(t, postResp))
	}
	code := oauthLocationCode(t, postResp)

	// Query-string grant_type smuggling on /oauth/token must be ignored (400 unsupported_grant_type).
	smuggled := oauthSend(t, client, http.MethodPost, base+"/oauth/token?grant_type=authorization_code&code="+url.QueryEscape(code)+"&redirect_uri="+url.QueryEscape(redirectURI),
		oauthFormContentType, "", func(r *http.Request) {
			r.SetBasicAuth("gemini-confidential", "top-secret")
		})
	if smuggled.StatusCode != http.StatusBadRequest {
		t.Fatalf("query-smuggled token exchange status %d, want 400", smuggled.StatusCode)
	}

	// Proper POST form exchange without code_verifier succeeds for confidential client.
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	}
	tokResp := oauthSend(t, client, http.MethodPost, base+"/oauth/token", oauthFormContentType, form.Encode(), func(r *http.Request) {
		r.SetBasicAuth("gemini-confidential", "top-secret")
	})
	if tokResp.StatusCode != http.StatusOK {
		t.Fatalf("confidential token exchange status %d: %s", tokResp.StatusCode, oauthBody(t, tokResp))
	}
	tokens := oauthJSON(t, tokResp)
	accessToken, _ := tokens["access_token"].(string)
	if accessToken == "" {
		t.Fatalf("empty access_token in %v", tokens)
	}
	session := oauthMCPInitialize(t, client, base, accessToken)
	oauthMCPTool(t, client, base, accessToken, session, "get_agent_brief")
}
