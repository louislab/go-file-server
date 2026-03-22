package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go-file-server/internal/db"
	"go-file-server/internal/httpserver"
	"go-file-server/internal/storage"
)

func main() {
	publicAddr := flag.String("public-addr", ":8080", "address for the LAN upload server")
	adminAddr := flag.String("admin-addr", "127.0.0.1:8081", "address for the localhost admin server")
	uploadDir := flag.String("upload-dir", "./uploads", "directory for uploaded files")
	dbPath := flag.String("db-path", "./data/app.db", "path to the SQLite database")
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

	publicURL := displayPublicURL(*publicAddr)

	app := httpserver.New(httpserver.Config{
		PublicAddr: *publicAddr,
		PublicURL:  publicURL,
		AdminAddr:  *adminAddr,
		Repository: repo,
		Storage:    store,
		Logger:     log.Default(),
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
	log.Printf("admin server listening on %s", displayAdminURL(*adminAddr))

	errCh := make(chan error, 2)
	go func() {
		errCh <- serve(publicServer)
	}()
	go func() {
		errCh <- serve(adminServer)
	}()

	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

func displayPublicURL(addr string) string {
	host, port := splitAddr(addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		if lanIP := detectLANIPv4(); lanIP != "" {
			return fmt.Sprintf("http://%s:%s", lanIP, port)
		}
		return fmt.Sprintf("http://localhost:%s", port)
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}

func displayAdminURL(addr string) string {
	host, port := splitAddr(addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}

func splitAddr(addr string) (string, string) {
	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		return host, port
	}

	trimmed := strings.TrimSpace(addr)
	if strings.HasPrefix(trimmed, ":") {
		return "", strings.TrimPrefix(trimmed, ":")
	}

	lastColon := strings.LastIndex(trimmed, ":")
	if lastColon > 0 && lastColon < len(trimmed)-1 {
		return trimmed[:lastColon], trimmed[lastColon+1:]
	}

	return trimmed, "80"
}

func detectLANIPv4() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			return ip.String()
		}
	}

	return ""
}
