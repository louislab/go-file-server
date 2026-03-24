package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go-file-server/internal/db"
	"go-file-server/internal/httpserver"
	"go-file-server/internal/netstate"
	"go-file-server/internal/storage"
)

func main() {
	publicAddr := flag.String("public-addr", ":8080", "address for the LAN upload server")
	adminAddr := flag.String("admin-addr", "127.0.0.1:8081", "address for the localhost admin server")
	uploadDir := flag.String("upload-dir", "./uploads", "directory for uploaded files")
	dbPath := flag.String("db-path", "./data/app.db", "path to the SQLite database")
	uploadSessionTTL := flag.Duration("upload-session-ttl", 24*time.Hour, "how long an incomplete resumable upload session can stay idle before cleanup")
	uploadCleanupInterval := flag.Duration("upload-cleanup-interval", 15*time.Minute, "how often stale resumable upload sessions are cleaned up")
	flag.Parse()

	if err := os.MkdirAll(*uploadDir, 0o755); err != nil {
		log.Fatalf("create upload dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		log.Fatalf("create database dir: %v", err)
	}

	repo, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer repo.Close()

	store, err := storage.New(*uploadDir)
	if err != nil {
		log.Fatalf("init storage: %v", err)
	}
	if err := cleanupStaleUploadSessions(context.Background(), repo, store, *uploadSessionTTL, log.Default()); err != nil {
		log.Printf("initial upload session cleanup failed: %v", err)
	}

	publicURL := netstate.DisplayPublicURL(*publicAddr)

	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app := httpserver.New(httpserver.Config{
		PublicAddr:       *publicAddr,
		AdminAddr:        *adminAddr,
		UploadSessionTTL: *uploadSessionTTL,
		Repository:       repo,
		Storage:          store,
		Logger:           log.Default(),
	})

	publicServer := &http.Server{
		Addr:              *publicAddr,
		Handler:           app.PublicHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	adminServer := &http.Server{
		Addr:              *adminAddr,
		Handler:           app.AdminHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("upload server listening on %s", publicURL)
	if hostInfo := netstate.SnapshotHTTP(*publicAddr); hostInfo.BonjourURL != "" {
		log.Printf("bonjour hostname available at %s", hostInfo.BonjourURL)
	}
	log.Printf("admin server listening on %s", displayAdminURL(*adminAddr))

	errCh := make(chan error, 2)
	go func() {
		errCh <- serve(publicServer)
	}()
	go func() {
		errCh <- serve(adminServer)
	}()
	go runUploadCleanupLoop(signalCtx, repo, store, *uploadSessionTTL, *uploadCleanupInterval, log.Default())

	select {
	case err := <-errCh:
		if err != nil {
			log.Fatalf("server error: %v", err)
		}
	case <-signalCtx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := publicServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("public server shutdown: %v", err)
	}
	if err := adminServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("admin server shutdown: %v", err)
	}
}

func serve(server *http.Server) error {
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func displayAdminURL(addr string) string {
	host, port := netstate.SplitListenAddr(addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}

func runUploadCleanupLoop(ctx context.Context, repo *db.Repository, store *storage.Service, ttl time.Duration, interval time.Duration, logger *log.Logger) {
	if ttl <= 0 || interval <= 0 {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := cleanupStaleUploadSessions(ctx, repo, store, ttl, logger); err != nil && logger != nil {
				logger.Printf("upload session cleanup failed: %v", err)
			}
		}
	}
}

func cleanupStaleUploadSessions(ctx context.Context, repo *db.Repository, store *storage.Service, ttl time.Duration, logger *log.Logger) error {
	if ttl <= 0 {
		return nil
	}

	cutoff := time.Now().UTC().Add(-ttl)
	sessions, err := repo.ListStaleUploadSessions(ctx, cutoff)
	if err != nil {
		return err
	}

	for _, session := range sessions {
		if err := store.DeletePartial(session.StoredPath); err != nil {
			return err
		}
		if err := repo.DeleteUploadSession(ctx, session.ID); err != nil && !errors.Is(err, db.ErrUploadSessionNotFound) {
			return err
		}
		if logger != nil {
			logger.Printf("cleaned stale upload session %s (%s)", session.ID, session.OriginalName)
		}
	}

	return nil
}
