package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"katchup/internal/account"
	"katchup/internal/api"
	"katchup/internal/config"
	"katchup/internal/crypto"
	"katchup/internal/imap"
	"katchup/internal/mcpserver"
	"katchup/internal/search"
	"katchup/internal/view/static"
	migrations "katchup/sql/migrations"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

func main() {
	slog.Info("katchup starting")

	ctx := context.Background()
	cfg := config.Load()

	// Configure structured logging
	configureLogger(cfg.Environment)

	// Create the data dir up front: SQLite won't create missing parent
	// directories, so a fresh checkout (`go run`) would otherwise fail to open.
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0700); err != nil {
		slog.Error("failed to create data directory", "path", filepath.Dir(cfg.DBPath), "error", err)
		os.Exit(1)
	}

	// Open SQLite database. modernc/sqlite applies _pragma to every new
	// pooled connection — a plain `PRAGMA` Exec only reaches one connection.
	// busy_timeout (first, so it also covers the pragmas after it) makes a
	// connection wait for a concurrent writer — sync vs. UI/API writes on the
	// pool — instead of failing at once with "database is locked".
	db, err := sql.Open("sqlite", cfg.DBPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		slog.Error("failed to ping database", "error", err)
		os.Exit(1)
	}

	if err := gooseUp(db); err != nil {
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}
	slog.Info("database migrated", "path", cfg.DBPath)

	// Initialize encryption wrapper
	var keyWrapper crypto.KeyWrapper
	if cfg.MasterKey != "" {
		keyWrapper, err = crypto.NewMasterKeyWrapper(cfg.MasterKey)
		if err != nil {
			slog.Error("failed to initialize master key wrapper", "error", err)
			os.Exit(1)
		}
		slog.Info("encryption enabled with master key")
	} else {
		slog.Warn("KATCHUP_MASTER_KEY not set — emails will NOT be encrypted at rest")
	}

	// Initialize account store with master key
	store, err := account.New(db, cfg.MasterKey)
	if err != nil {
		slog.Error("failed to initialize account store", "error", err)
		os.Exit(1)
	}

	// Initialize IMAP sync
	dataDir := filepath.Dir(cfg.DBPath)
	imapStore, err := imap.NewStore(db, store)
	if err != nil {
		slog.Error("failed to initialize imap store", "error", err)
		os.Exit(1)
	}
	syncer := imap.NewSyncer(imapStore, dataDir, keyWrapper, store)
	syncer.ThrottleCooldown = cfg.ThrottleCooldown
	syncer.FetchPacing = cfg.FetchPacing

	// Adding, removing, or changing KATCHUP_MASTER_KEY after data exists
	// leaves stored passwords and blobs unreadable. Say so loudly at startup
	// rather than as a trail of sync/decrypt failures.
	if problems, err := syncer.CheckMasterKey(ctx); err != nil {
		slog.Warn("master key consistency check failed", "error", err)
	} else if len(problems) > 0 {
		slog.Error("KATCHUP_MASTER_KEY does not match the existing data — restore the key this archive was written with",
			"problems", len(problems))
		for _, p := range problems {
			slog.Error("master key mismatch: " + p)
		}
	}

	// Header-only search backend (S10). When MEILI_URL is set, messages are
	// indexed on store and /search queries Meilisearch; otherwise search degrades
	// to a SQLite LIKE over subject/from (no hard dependency). Only headers are
	// ever indexed — bodies stay encrypted.
	var searcher search.Searcher
	if cfg.MeiliURL != "" {
		meili := search.NewMeili(cfg.MeiliURL, cfg.MeiliKey)
		if err := meili.EnsureIndex(ctx); err != nil {
			// Without the index settings, filtered (account/date) searches fail
			// and fall back to the DB — so keep trying rather than giving up.
			slog.Warn("failed to initialize Meilisearch index; retrying in the background, search falls back to DB until then", "error", err)
			go ensureIndexRetry(ctx, meili)
		}
		imapStore.SetIndexer(meili)
		searcher = meili
		slog.Info("header-only search enabled via Meilisearch", "url", cfg.MeiliURL)
	} else {
		slog.Info("MEILI_URL not set — /search uses SQLite LIKE fallback")
	}

	// Subcommand: `katchup backfill` rebuilds the blobs/messages/search index
	// from the encrypted .eml files already on disk, WITHOUT contacting IMAP,
	// then exits. Use it to recover after a lost/reset DB without re-downloading
	// (and re-triggering provider bandwidth throttling). Runs after the search
	// backend is wired so it also repopulates Meilisearch.
	if len(os.Args) > 1 && os.Args[1] == "backfill" {
		slog.Info("running local backfill from disk (no IMAP)")
		if err := syncer.Backfill(ctx); err != nil {
			slog.Error("backfill failed", "error", err)
			os.Exit(1)
		}
		slog.Info("backfill complete")
		os.Exit(0)
	}

	// Subcommand: `katchup reindex` re-pushes every stored message's header-only
	// doc to Meilisearch (no decrypt, no IMAP), then exits. Use it to populate
	// search from an existing archive — e.g. after a backfill that ran without
	// Meili reachable.
	if len(os.Args) > 1 && os.Args[1] == "reindex" {
		slog.Info("reindexing search from existing messages")
		if cfg.MeiliURL == "" {
			slog.Error("reindex requires MEILI_URL to be set")
			os.Exit(1)
		}
		n, err := imapStore.ReindexAll(ctx)
		if err != nil {
			slog.Error("reindex failed", "error", err, "indexed", n)
			os.Exit(1)
		}
		slog.Info("reindex complete", "indexed", n)
		os.Exit(0)
	}

	// On startup, mark any stale "running" sync runs as "failed"
	// so accounts aren't permanently blocked from syncing.
	if err := syncer.MarkAllStaleRuns(ctx); err != nil {
		slog.Warn("failed to clear stale runs at startup", "error", err)
	}

	// Start the scheduled-sync ticker. This is a SAFETY-NET FLOOR: an external
	// agent drives freshness via POST /api/sync, but the floor guarantees a backup
	// even if it is down. Interval is configurable via KATCHUP_SYNC_INTERVAL (6h).
	syncCtx, syncCancel := context.WithCancel(ctx)
	go func() {
		// Run immediately on startup
		slog.Info("running initial sync")
		syncer.SyncAll(syncCtx)

		ticker := time.NewTicker(cfg.SyncInterval)
		defer ticker.Stop()

		for {
			select {
			case <-syncCtx.Done():
				return
			case <-ticker.C:
				slog.Info("running scheduled sync", "interval", cfg.SyncInterval)
				syncer.SyncAll(syncCtx)
			}
		}
	}()

	// HTTP server
	mux := http.NewServeMux()

	// Register health endpoint
	mux.HandleFunc("GET /health", healthHandler)

	// Vendored assets (Tailwind runtime + fonts), embedded in the binary —
	// pages make no external network requests.
	mux.Handle("GET /static/", static.Handler())

	// Root path renders the dashboard (sync status overview).
	syncHandler := api.NewSyncHandler(store, syncer)
	mux.HandleFunc("GET /{$}", syncHandler.Status)

	// Register account handlers
	acctHandler := api.NewAccountHandler(store)
	mux.Handle("/accounts", acctHandler)
	mux.Handle("/accounts/new", acctHandler)
	mux.Handle("/accounts/{id}/edit", acctHandler)
	mux.Handle("/accounts/{id}/delete", acctHandler)

	// Register sync handlers
	mux.Handle("/sync", syncHandler)
	mux.Handle("/sync/{id}/trigger", syncHandler)

	// Register browse handler
	browseHandler := api.NewBrowseHandler(store, imapStore, dataDir, keyWrapper)
	mux.Handle("/browse", browseHandler)
	mux.HandleFunc("GET /browse/download/{id}", browseHandler.Download)

	// Register header-only search handler (Meili when configured, DB LIKE fallback).
	searchHandler := api.NewSearchHandler(store, imapStore, searcher)
	mux.Handle("/search", searchHandler)

	// Register machine-facing API (/api/*). Wrapped in token auth that fails
	// closed: if KATCHUP_API_TOKEN is unset, every /api/* route returns 503.
	apiMux := http.NewServeMux()
	archivedHandler := api.NewArchivedHandler(imapStore)
	apiMux.HandleFunc("GET /api/archived", archivedHandler.Get)
	apiMux.HandleFunc("POST /api/archived/lookup", archivedHandler.Lookup)
	// On-demand sync trigger (an agent pokes katchup before polling). Async 202 with
	// the run id; per-account mutex + coalesce window guard against stampede.
	syncTriggerHandler := api.NewSyncTriggerHandler(store, syncer, cfg.CoalesceWindow)
	apiMux.HandleFunc("POST /api/sync", syncTriggerHandler.Trigger)
	// MCP server (stateless streamable HTTP) for agents: same token, plus
	// get_message, which returns decrypted bodies.
	apiMux.Handle("/api/mcp", mcpserver.Handler(mcpserver.Deps{
		Accounts:       store,
		Messages:       imapStore,
		Syncer:         syncer,
		Searcher:       searcher,
		DataDir:        dataDir,
		KeyWrapper:     keyWrapper,
		CoalesceWindow: cfg.CoalesceWindow,
	}))
	mux.Handle("/api/", api.APIAuth(cfg.APIToken, apiMux))
	if cfg.APIToken == "" {
		slog.Warn("KATCHUP_API_TOKEN not set — /api/* routes are disabled (503)")
	}

	// Web-UI auth: access key from KATCHUP_UI_KEY, or created on first visit
	// and stored (hashed) in the DB. Wraps every route except /health, /login,
	// and the separately token-guarded /api/*. The session cookie is
	// SameSite=Lax, which also blocks cross-site request forgery against the
	// state-changing POST routes.
	uiAuth := api.NewUIAuth(db, cfg.UIKey)
	uiAuth.RegisterRoutes(mux)

	// Bound how long a client may take to send a request, and how long an
	// idle keep-alive connection is held, so slow or stalled clients can't pin
	// connections. No WriteTimeout: .eml downloads and MCP responses can be
	// large on a slow link.
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           uiAuth.Middleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		slog.Info("starting HTTP server", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	slog.Info("shutting down server...")
	syncCancel()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}

	slog.Info("katchup stopped")
}

// gooseUp runs goose up using migrations embedded in the binary,
// so startup does not depend on the working directory.
func gooseUp(db *sql.DB) error {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.Up(db, ".")
}

// ensureIndexRetry retries Meilisearch index setup with capped exponential
// backoff until it succeeds or ctx ends.
func ensureIndexRetry(ctx context.Context, meili *search.Meili) {
	delay := 5 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if err := meili.EnsureIndex(ctx); err != nil {
			slog.Warn("Meilisearch index setup still failing", "error", err, "retry_in", delay)
			delay = min(delay*2, 5*time.Minute)
			continue
		}
		slog.Info("Meilisearch index initialized")
		return
	}
}

// configureLogger sets up slog with JSON handler for production or text handler for development.
func configureLogger(env string) {
	var handler slog.Handler
	if env == "production" {
		handler = slog.NewJSONHandler(os.Stdout, nil)
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})
	}
	slog.SetDefault(slog.New(handler))
}

// healthHandler returns a JSON health check response.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}
