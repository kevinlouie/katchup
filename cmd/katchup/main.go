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

	// Open SQLite database. modernc/sqlite applies _pragma to every new
	// pooled connection — a plain `PRAGMA` Exec only reaches one connection.
	db, err := sql.Open("sqlite", cfg.DBPath+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
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

	// FIX #9: On startup, mark any stale "running" sync runs as "failed"
	// so accounts aren't permanently blocked from syncing.
	if err := syncer.MarkAllStaleRuns(ctx); err != nil {
		slog.Warn("failed to clear stale runs at startup", "error", err)
	}

	// Start IMAP sync ticker (every 15 minutes)
	syncCtx, syncCancel := context.WithCancel(ctx)
	go func() {
		// Run immediately on startup
		slog.Info("running initial sync")
		syncer.SyncAll(syncCtx)

		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-syncCtx.Done():
				return
			case <-ticker.C:
				slog.Info("running scheduled sync")
				syncer.SyncAll(syncCtx)
			}
		}
	}()

	// HTTP server
	mux := http.NewServeMux()

	// Register health endpoint
	mux.HandleFunc("GET /health", healthHandler)

	// Register account handlers
	acctHandler := api.NewAccountHandler(store)
	mux.Handle("/accounts", acctHandler)
	mux.Handle("/accounts/new", acctHandler)
	mux.Handle("/accounts/{id}/edit", acctHandler)
	mux.Handle("/accounts/{id}/delete", acctHandler)

	// Register sync handlers
	syncHandler := api.NewSyncHandler(store, syncer)
	mux.Handle("/sync", syncHandler)
	mux.Handle("/sync/{id}/trigger", syncHandler)

	// Register browse handler
	browseHandler := api.NewBrowseHandler(store, dataDir, keyWrapper)
	mux.Handle("/browse", browseHandler)

	// Register download endpoints
	dlHandler := api.NewDownloadHandler(store, imapStore, syncer, dataDir, keyWrapper)
	mux.HandleFunc("GET /download/{accountID}/{date}/{filename}", dlHandler.Handle)
	mux.HandleFunc("GET /browse/{accountID}/{date}/{filename}", browseHandler.Download)

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: mux,
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
