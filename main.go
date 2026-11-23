package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "1.2.5"

// versionString is the one-line build identity printed by --version. It reads
// the same version constant the MCP serverInfo and startup log use, so a
// deployment check cannot disagree with the running handshake.
func versionString() string { return "agent-mcp-go " + version }

type App struct {
	config        *ConfigStore
	token         []byte
	oauth         *oauthServer
	sessions      *SessionManager
	transfers     *TransferManager
	projectGuards projectLockPool
	// stateGuards serializes all per-scope task state work (notes, plans,
	// handover snapshots and the ephemeral marker). Locks are keyed by the
	// resolved scope directory path, so independent scopes stay concurrent
	// while the state files of one scope are never observed halfway through
	// an update and the cleanup sweep can never race a live writer.
	stateGuards  projectLockPool
	scopeSweepAt atomic.Int64
	// skillsCache reuses parsed metadata while skill names, file metadata,
	// and bounded frontmatter match the current catalog fingerprint.
	skillsCache skillCatalogCache
	// reviewBaselineCooldown suppresses repeated full snapshot attempts for a
	// repository that recently timed out or matched review_baseline_skip_paths.
	reviewBaselineCooldown reviewBaselineCooldownState
	requestLogger          *log.Logger
}

type mcpHTTPModeKey struct{}

func markMCPHTTPMode(mode string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mcpHTTPModeKey{}, mode)))
	})
}

func newApp(configPath string) (*App, error) {
	config, err := newConfigStore(configPath)
	if err != nil {
		return nil, err
	}
	c := config.Get()
	token, err := readBearerToken(c.TokenFile)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		return nil, err
	}
	// Transfer tickets are process-local. After a restart, any old export
	// snapshots are unreachable, so clean only our dedicated snapshot files.
	if err = cleanupOrphanExports(c.StateDir); err != nil {
		return nil, err
	}
	sessions := NewSessionManager()
	sessions.yieldBounds = func() (int, int) {
		c := config.Get()
		return c.DefaultYieldMS, c.MaxYieldMS
	}
	app := &App{
		config:    config,
		token:     token,
		sessions:  sessions,
		transfers: NewTransferManager(),
	}
	if c.OAuth != nil && c.OAuth.Enabled {
		oauth, err := newOAuthServer(app)
		if err != nil {
			return nil, err
		}
		app.oauth = oauth
	}
	// Task state of expired anonymous MCP sessions is disposable; explicit
	// and ChatGPT scopes are durable and are never touched by this sweep.
	// A zero grace period also reclaims empty orphan directories left behind
	// by an earlier crash, before the server accepts any traffic.
	if err = app.cleanupStaleScopes(context.Background(), c.StateDir, ephemeralSessionTTL, 0); err != nil {
		return nil, err
	}
	return app, nil
}

// auth enforces RFC 9728 Bearer authentication (accepting both the owner
// static token and, when enabled, issued OAuth 2.1 access tokens) followed by
// Host and Origin guards. Authentication runs first so unauthenticated probes
// always receive 401 + WWW-Authenticate rather than Host/Origin disclosures.
func (a *App) auth(next http.Handler) http.Handler {
	bearer := auth.RequireBearerToken(a.verifyBearerToken, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: a.resourceMetadataURL(),
		Scopes:              []string{"mcp"},
		ClockSkew:           30 * time.Second,
	})
	guarded := bearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.checkHostAndOrigin(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			a.emitOAuthProbe("mcp_challenge", r.Method, http.StatusUnauthorized, "missing_bearer")
		}
		guarded.ServeHTTP(w, r)
	})
}

// checkHost enforces the exact configured Host allowlist. A reverse proxy
// preserves the external Host header, so the SDK's loopback-only Host rule is
// replaced by this check.
func (a *App) checkHost(w http.ResponseWriter, r *http.Request) bool {
	c := a.config.Get()
	if r.Host == c.Listen {
		return true
	}
	if public, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(c.PublicURL), "/")); err == nil &&
		public.Host != "" && strings.EqualFold(r.Host, public.Host) {
		return true
	}
	http.Error(w, "host denied", http.StatusForbidden)
	return false
}

// checkHostAndOrigin runs both browser-facing guards for authenticated MCP
// traffic. Origin checks block browser DNS-rebinding from smuggling tool
// calls: only the configured public origin (and explicitly allowed remote
// client origins) may carry authenticated tool calls. A request with more than
// one Origin header is ambiguous and is rejected.
func (a *App) checkHostAndOrigin(w http.ResponseWriter, r *http.Request) bool {
	if !a.checkHost(w, r) {
		return false
	}
	c := a.config.Get()
	if origins := r.Header.Values("Origin"); len(origins) > 1 {
		w.Header().Add("Vary", "Origin")
		http.Error(w, "origin denied", http.StatusForbidden)
		return false
	} else if len(origins) == 1 && origins[0] != "" && !c.allowsOrigin(origins[0]) {
		w.Header().Add("Vary", "Origin")
		http.Error(w, "origin denied", http.StatusForbidden)
		return false
	}
	return true
}

// resourceMetadataURL is the RFC 9728 metadata location advertised in the
// WWW-Authenticate challenge of protected MCP endpoints.
func (a *App) resourceMetadataURL() string {
	if a.oauth == nil {
		return ""
	}
	return a.oauth.publicBase() + "/.well-known/oauth-protected-resource"
}

// verifyBearerToken accepts the static owner token and, when OAuth is enabled,
// a live OAuth access token. The UserID is stable per client so a stateful
// Mcp-Session-Id survives hourly access-token refreshes while still refusing
// cross-client session hijacking.
func (a *App) verifyBearerToken(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	if tokenEqual(token, a.token) {
		return &auth.TokenInfo{
			UserID:     "owner:bearer",
			Scopes:     []string{"mcp"},
			Expiration: time.Now().Add(24 * time.Hour),
		}, nil
	}
	if a.oauth != nil {
		if rec, err := a.oauth.verifyAccessToken(token); err == nil {
			return &auth.TokenInfo{
				UserID:     "oauth:" + rec.ClientID,
				Scopes:     rec.Scopes,
				Expiration: rec.ExpiresAt,
			}, nil
		}
	}
	return nil, auth.ErrInvalidToken
}

func (a *App) handler() http.Handler {
	capabilities := &mcp.ServerCapabilities{Logging: &mcp.LoggingCapabilities{}}
	capabilities.AddExtension(reviewExtension, nil)
	server := mcp.NewServer(&mcp.Implementation{Name: "agent-mcp-go", Version: version},
		&mcp.ServerOptions{Instructions: a.buildAgentInstructions(), Capabilities: capabilities})
	a.registerCore(server)
	a.registerEnvironmentParity(server)
	a.registerSearchParity(server)
	a.registerPatchParity(server)
	a.registerShared(server)
	a.registerTransfer(server)
	a.registerParity(server)
	a.registerMCPParity(server)
	a.registerFileParity(server)
	a.registerChatGPTImport(server)
	a.registerExportResources(server)
	a.registerReviewUI(server)
	server.AddReceivingMiddleware(a.requestDiagnosticsMiddleware, a.refreshInstructionsMiddleware, a.reviewLifecycleMiddleware, requestDispatchMiddleware, a.toolContractsMiddleware)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	statefulHandler := a.auth(a.requestHTTPDiagnostics("stateful", markMCPHTTPMode("stateful", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{JSONResponse: true, DisableLocalhostProtection: true}))))
	mux.Handle("/mcp", statefulHandler)
	// Gemini Web / Connected Apps probes HEAD / and POST / at the origin root
	// during URL verification; mounting /{$} exposes the same authenticated
	// Streamable HTTP contract and RFC 9728 401 challenge at bare public_url.
	mux.Handle("/{$}", statefulHandler)
	// Some MCP clients do not preserve Mcp-Session-Id across their HTTP
	// requests. Provide an opt-in sessionless endpoint using the official SDK,
	// without weakening or changing the existing stateful /mcp contract.
	// Tools that store task-local scratch state still require an explicit scope.
	mux.Handle("/mcp/stateless", a.auth(a.requestHTTPDiagnostics("stateless", markMCPHTTPMode("stateless", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: true})))))
	a.registerOAuth(mux)
	mux.Handle("/transfer/", a.transfers.handler(a))
	return a.withMCPCORS(mux)
}

// isMCPCORSPath reports whether path is an MCP transport entry point. Browser
// hosts reach the same Streamable HTTP contract through /mcp, the sessionless
// /mcp/stateless variant, and the bare public root.
func isMCPCORSPath(path string) bool {
	switch path {
	case "/", "/mcp", "/mcp/stateless":
		return true
	}
	return false
}

// withMCPCORS is a thin browser-facing wrapper around the MCP transport
// endpoints. It is inert unless allowed_origins is configured, so the previous
// contract is unchanged for every caller that does not opt in.
//
// A browser preflight never carries credentials, so it is answered before
// bearer authentication: an allowlisted origin gets a sessionless 204 with the
// MCP request headers, while any other origin (including a missing, repeated or
// non-allowlisted Origin) is rejected without a CORS grant. Actual MCP
// responses from an allowlisted origin advertise the response headers a
// browser host needs to read, and credentials are never allowed.
func (a *App) withMCPCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := a.config.Get()
		if len(c.AllowedOrigins) == 0 || !isMCPCORSPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		origin := ""
		if origins := r.Header.Values("Origin"); len(origins) == 1 {
			origin = strings.TrimSpace(origins[0])
		}
		if r.Method == http.MethodOptions {
			if origin == "" || !c.allowsOrigin(origin) {
				w.Header().Add("Vary", "Origin")
				http.Error(w, "origin denied", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "POST, GET, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-ID")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if origin != "" && c.allowsOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id, WWW-Authenticate")
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	path := flag.String("config", os.Getenv("AGENT_MCP_GO_CONFIG"), "Path to agent-mcp-go JSON config")
	showVersion := flag.Bool("version", false, "Print the agent-mcp-go version and exit")
	flag.Parse()
	// --version must answer without a configuration file: it proves which
	// binary is on disk, so it runs before the config requirement.
	if *showVersion {
		fmt.Println(versionString())
		return
	}
	if *path == "" {
		log.Fatal("set --config or AGENT_MCP_GO_CONFIG")
	}
	app, err := newApp(*path)
	if err != nil {
		log.Fatal(err)
	}
	app.requestLogger = log.Default()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				app.transfers.sweepExpired()
			}
		}
	}()
	c := app.config.Get()
	log.Printf("agent-mcp-go %s listening on %s (loopback only)", version, c.Listen)
	srv := &http.Server{
		Addr: c.Listen, Handler: app.handler(), ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes: 16 * 1024, IdleTimeout: 120 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.ListenAndServe() }()
	select {
	case err = <-serveDone:
	case <-ctx.Done():
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), executionShutdownTimeout)
	defer cancel()
	httpDone := make(chan error, 1)
	go func() { httpDone <- srv.Shutdown(shutdownCtx) }()
	if shutdownErr := app.sessions.shutdown(); shutdownErr != nil {
		log.Printf("shutdown: %v", shutdownErr)
	}
	if shutdownErr := <-httpDone; shutdownErr != nil {
		_ = srv.Close()
		log.Printf("HTTP shutdown: %v", shutdownErr)
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
