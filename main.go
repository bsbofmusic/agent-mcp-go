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
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "1.0.0"

type App struct {
	config        *ConfigStore
	token         []byte
	sessions      *SessionManager
	transfers     *TransferManager
	catalog       routeCatalog
	projectGuards projectLockPool
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
	return &App{
		config:    config,
		token:     token,
		sessions:  NewSessionManager(),
		transfers: NewTransferManager(),
	}, nil
}

func (a *App) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("Authorization")
		if !strings.HasPrefix(v, "Bearer ") || !tokenEqual(strings.TrimPrefix(v, "Bearer "), a.token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c := a.config.Get()
		// A reverse proxy preserves the external Host header. Replace the
		// SDK's loopback-only Host rule with an exact configured host allowlist.
		public, _ := url.Parse(c.PublicURL)
		if r.Host != c.Listen && (public == nil || !strings.EqualFold(r.Host, public.Host)) {
			http.Error(w, "host denied", http.StatusForbidden)
			return
		}
		// Origin checks block browser DNS-rebinding from smuggling tool calls.
		if origin := r.Header.Get("Origin"); origin != "" {
			if origin != strings.TrimSuffix(c.PublicURL, "/") {
				http.Error(w, "origin denied", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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
	server.AddReceivingMiddleware(a.refreshInstructionsMiddleware, a.reviewLifecycleMiddleware, toolContractsMiddleware)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("/mcp", a.auth(markMCPHTTPMode("stateful", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{JSONResponse: true, DisableLocalhostProtection: true}))))
	// Some MCP clients do not preserve Mcp-Session-Id across their HTTP
	// requests. Provide an opt-in sessionless endpoint using the official SDK,
	// without weakening or changing the existing stateful /mcp contract.
	// Tools that store task-local scratch state still require an explicit scope.
	mux.Handle("/mcp/stateless", a.auth(markMCPHTTPMode("stateless", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: true}))))
	mux.Handle("/transfer/", a.transfers.handler(a))
	return mux
}

func main() {
	path := flag.String("config", os.Getenv("AGENT_MCP_GO_CONFIG"), "Path to agent-mcp-go JSON config")
	flag.Parse()
	if *path == "" {
		log.Fatal("set --config or AGENT_MCP_GO_CONFIG")
	}
	app, err := newApp(*path)
	if err != nil {
		log.Fatal(err)
	}
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
