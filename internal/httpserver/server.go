package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"go-file-server/internal/db"
	"go-file-server/internal/storage"
)

type Config struct {
	PublicAddr string
	PublicURL  string
	AdminAddr  string
	Repository *db.Repository
	Storage    *storage.Service
	Logger     *log.Logger
}

type Server struct {
	repo      *db.Repository
	storage   *storage.Service
	logger    *log.Logger
	assets    fs.FS
	publicURL string
}

func New(config Config) *Server {
	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}

	return &Server{
		repo:      config.Repository,
		storage:   config.Storage,
		logger:    logger,
		assets:    mustAssetFS(logger),
		publicURL: strings.TrimRight(strings.TrimSpace(config.PublicURL), "/"),
	}
}

func (s *Server) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.assets))))
	mux.HandleFunc("/api/upload", s.handleUpload)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/", s.serveUploadPage)
	return s.loggingMiddleware("public", mux)
}

func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.assets))))
	mux.HandleFunc("/api/host", s.handleHostInfo)
	mux.HandleFunc("/api/files", s.handleFilesCollection)
	mux.HandleFunc("/api/files/", s.handleFileItem)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/", s.serveAdminPage)
	return s.loggingMiddleware("admin", mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *Server) serveUploadPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	s.serveAsset(w, "upload.html", "text/html; charset=utf-8")
}

func (s *Server) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	s.serveAsset(w, "admin.html", "text/html; charset=utf-8")
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	reader, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid multipart request"})
		return
	}

	clientIP := clientIPFromRequest(r.RemoteAddr)
	hostname := lookupHostname(r.Context(), clientIP)
	userAgent := r.UserAgent()

	type uploadedFile struct {
		ID           string `json:"id"`
		OriginalName string `json:"originalName"`
		FileSize     int64  `json:"fileSize"`
		MIMEType     string `json:"mimeType"`
		UploadedAt   string `json:"uploadedAt"`
	}

	results := make([]uploadedFile, 0)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read multipart body"})
			return
		}

		if part.FileName() == "" {
			_ = part.Close()
			continue
		}

		id := uuid.NewString()
		now := time.Now().UTC()
		saved, err := s.storage.Save(id, part.FileName(), part.Header.Get("Content-Type"), part)
		_ = part.Close()
		if err != nil {
			s.logger.Printf("save upload: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save upload to disk"})
			return
		}

		record := db.FileRecord{
			ID:              id,
			OriginalName:    part.FileName(),
			StoredPath:      saved.StoredPath,
			FileSize:        saved.FileSize,
			MIMEType:        saved.MIMEType,
			UploadedAt:      now,
			ClientIP:        clientIP,
			ClientHostname:  hostname,
			ClientUserAgent: userAgent,
		}

		if err := s.repo.InsertFile(r.Context(), record); err != nil {
			s.logger.Printf("insert file record: %v", err)
			_ = s.storage.Delete(saved.StoredPath)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save upload metadata"})
			return
		}

		results = append(results, uploadedFile{
			ID:           id,
			OriginalName: record.OriginalName,
			FileSize:     saved.FileSize,
			MIMEType:     saved.MIMEType,
			UploadedAt:   now.Format(time.RFC3339),
		})
	}

	if len(results) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no files in upload request"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"files": results})
}

func (s *Server) handleFilesCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	records, err := s.repo.ListFiles(r.Context())
	if err != nil {
		s.logger.Printf("list files: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "list files"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"files": records})
}

func (s *Server) handleHostInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"uploadURL": s.publicURL,
	})
}

func (s *Server) handleFileItem(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/files/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	id := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodDelete {
			methodNotAllowed(w)
			return
		}
		s.handleDelete(w, r, id)
		return
	}

	if len(parts) == 2 && parts[1] == "view" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handleView(w, r, id)
		return
	}

	http.NotFound(w, r)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, id string) {
	record, err := s.repo.GetFile(r.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "file not found"})
			return
		}
		s.logger.Printf("load file for delete: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "load file"})
		return
	}

	if err := s.storage.Delete(record.StoredPath); err != nil {
		s.logger.Printf("delete stored file: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "delete file from disk"})
		return
	}

	if err := s.repo.DeleteFile(r.Context(), id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "file not found"})
			return
		}
		s.logger.Printf("delete file record: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "delete file metadata"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleView(w http.ResponseWriter, r *http.Request, id string) {
	record, err := s.repo.GetFile(r.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("load file for view: %v", err)
		http.Error(w, "failed to load file", http.StatusInternalServerError)
		return
	}

	file, _, err := s.storage.Open(record.StoredPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		s.logger.Printf("open file for view: %v", err)
		http.Error(w, "failed to open file", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		s.logger.Printf("stat file for view: %v", err)
		http.Error(w, "failed to inspect file", http.StatusInternalServerError)
		return
	}

	disposition := "attachment"
	if !requestWantsDownload(r) && canInline(record.MIMEType, record.OriginalName) {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", contentType(record.MIMEType, record.OriginalName))
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disposition, record.OriginalName))
	http.ServeContent(w, r, record.OriginalName, stat.ModTime(), file)
}

func requestWantsDownload(r *http.Request) bool {
	value := strings.TrimSpace(strings.ToLower(r.URL.Query().Get("download")))
	return value == "1" || value == "true" || value == "yes"
}

func (s *Server) serveAsset(w http.ResponseWriter, name string, contentType string) {
	content, err := fs.ReadFile(s.assets, name)
	if err != nil {
		http.Error(w, "asset not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(content)
}

func (s *Server) loggingMiddleware(name string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		s.logger.Printf("[%s] %s %s %s", name, r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
	})
}

func clientIPFromRequest(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func lookupHostname(parent context.Context, ip string) string {
	if ip == "" {
		return ""
	}

	ctx, cancel := context.WithTimeout(parent, 1200*time.Millisecond)
	defer cancel()

	type result struct {
		names []string
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		names, err := net.LookupAddr(ip)
		resultCh <- result{names: names, err: err}
	}()

	select {
	case <-ctx.Done():
		return ""
	case res := <-resultCh:
		if res.err != nil || len(res.names) == 0 {
			return ""
		}
		return strings.TrimSuffix(res.names[0], ".")
	}
}

func canInline(mimeType string, originalName string) bool {
	baseType := strings.ToLower(strings.TrimSpace(mimeType))
	if strings.HasPrefix(baseType, "image/") || strings.HasPrefix(baseType, "text/") {
		return true
	}
	if strings.HasPrefix(baseType, "audio/") || strings.HasPrefix(baseType, "video/") {
		return true
	}
	if baseType == "application/pdf" || baseType == "application/json" {
		return true
	}

	ext := strings.ToLower(filepath.Ext(originalName))
	return ext == ".pdf" || ext == ".txt" || ext == ".log" || ext == ".json"
}

func contentType(mimeType string, originalName string) string {
	if strings.TrimSpace(mimeType) != "" {
		return mimeType
	}
	if guessed := mime.TypeByExtension(strings.ToLower(filepath.Ext(originalName))); guessed != "" {
		return guessed
	}
	return "application/octet-stream"
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "failed to encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}
