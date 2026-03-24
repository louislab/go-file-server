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
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"go-file-server/internal/db"
	"go-file-server/internal/netstate"
	"go-file-server/internal/storage"
)

type Config struct {
	PublicAddr       string
	AdminAddr        string
	UploadSessionTTL time.Duration
	Repository       *db.Repository
	Storage          *storage.Service
	Logger           *log.Logger
}

type Server struct {
	repo             *db.Repository
	storage          *storage.Service
	logger           *log.Logger
	assets           fs.FS
	staticFS         fs.FS
	publicAddr       string
	uploadSessionTTL time.Duration
}

func New(config Config) *Server {
	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}

	staticFS, err := fs.Sub(mustAssetFS(logger), "static")
	if err != nil {
		logger.Fatalf("load static assets: %v", err)
	}

	return &Server{
		repo:             config.Repository,
		storage:          config.Storage,
		logger:           logger,
		assets:           mustAssetFS(logger),
		staticFS:         staticFS,
		publicAddr:       strings.TrimSpace(config.PublicAddr),
		uploadSessionTTL: config.UploadSessionTTL,
	}
}

func (s *Server) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.staticFS))))
	mux.HandleFunc("/api/upload/sessions", s.handleUploadSessions)
	mux.HandleFunc("/api/upload/sessions/", s.handleUploadSessionItem)
	mux.HandleFunc("/api/upload", s.handleUpload)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/", s.serveUploadPage)
	return s.loggingMiddleware("public", mux)
}

func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.staticFS))))
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
	hostInfo := netstate.SnapshotHTTP(s.publicAddr)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"uploadURL":          hostInfo.UploadURL,
		"bonjourURL":         hostInfo.BonjourURL,
		"activeAddressCount": len(hostInfo.Addresses),
		"network":            hostInfo,
	})
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

type uploadSessionInitRequest struct {
	OriginalName string `json:"originalName"`
	FileSize     int64  `json:"fileSize"`
	MIMEType     string `json:"mimeType"`
	LastModified int64  `json:"lastModified"`
	ResumeKey    string `json:"resumeKey"`
}

type uploadSessionPayload struct {
	ID            string `json:"id"`
	OriginalName  string `json:"originalName"`
	FileSize      int64  `json:"fileSize"`
	MIMEType      string `json:"mimeType"`
	BytesReceived int64  `json:"bytesReceived"`
	Complete      bool   `json:"complete"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

func (s *Server) handleUploadSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	var request uploadSessionInitRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid upload session request"})
		return
	}

	request.OriginalName = strings.TrimSpace(request.OriginalName)
	request.MIMEType = strings.TrimSpace(request.MIMEType)
	request.ResumeKey = normalizedResumeKey(request.ResumeKey, request.OriginalName, request.FileSize, request.LastModified)
	if request.OriginalName == "" || request.FileSize <= 0 || request.ResumeKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing resumable upload metadata"})
		return
	}

	if existing, err := s.repo.FindActiveUploadSessionByResumeKey(r.Context(), request.ResumeKey); err == nil {
		if s.isUploadSessionExpired(existing, time.Now().UTC()) {
			if cleanupErr := s.abandonUploadSession(r.Context(), existing); cleanupErr != nil {
				s.logger.Printf("cleanup expired upload session: %v", cleanupErr)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "cleanup expired upload session"})
				return
			}
		} else {
			existing = s.syncUploadSessionProgress(r.Context(), existing)
			writeJSON(w, http.StatusOK, map[string]any{"session": uploadSessionPayloadFromRecord(existing)})
			return
		}
	} else if !errors.Is(err, db.ErrUploadSessionNotFound) {
		s.logger.Printf("load upload session: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "load upload session"})
		return
	}

	uploadID := uuid.NewString()
	prepared, err := s.storage.PrepareResumable(uploadID, request.OriginalName)
	if err != nil {
		s.logger.Printf("prepare resumable upload: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "prepare upload"})
		return
	}

	clientIP := clientIPFromRequest(r.RemoteAddr)
	now := time.Now().UTC()
	session := db.UploadSession{
		ID:              uploadID,
		ResumeKey:       request.ResumeKey,
		OriginalName:    request.OriginalName,
		StoredPath:      prepared.StoredPath,
		FileSize:        request.FileSize,
		MIMEType:        request.MIMEType,
		ClientIP:        clientIP,
		ClientHostname:  lookupHostname(r.Context(), clientIP),
		ClientUserAgent: r.UserAgent(),
		BytesReceived:   0,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if err := s.repo.CreateUploadSession(r.Context(), session); err != nil {
		_ = s.storage.DeletePartial(prepared.StoredPath)
		s.logger.Printf("create upload session: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "create upload session"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"session": uploadSessionPayloadFromRecord(session)})
}

func (s *Server) handleUploadSessionItem(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/upload/sessions/"), "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleUploadSessionStatus(w, r, id)
	case http.MethodPut:
		s.handleUploadChunk(w, r, id)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleUploadSessionStatus(w http.ResponseWriter, r *http.Request, id string) {
	session, err := s.repo.GetUploadSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrUploadSessionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "upload session not found"})
			return
		}
		s.logger.Printf("get upload session: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "load upload session"})
		return
	}

	session = s.syncUploadSessionProgress(r.Context(), session)
	writeJSON(w, http.StatusOK, map[string]any{"session": uploadSessionPayloadFromRecord(session)})
}

func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request, id string) {
	session, err := s.repo.GetUploadSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrUploadSessionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "upload session not found"})
			return
		}
		s.logger.Printf("get upload session: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "load upload session"})
		return
	}
	if session.CompletedAt != nil {
		writeJSON(w, http.StatusOK, map[string]any{"session": uploadSessionPayloadFromRecord(session)})
		return
	}
	if s.isUploadSessionExpired(session, time.Now().UTC()) {
		if cleanupErr := s.abandonUploadSession(r.Context(), session); cleanupErr != nil {
			s.logger.Printf("cleanup expired upload session: %v", cleanupErr)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "cleanup expired upload session"})
			return
		}
		writeJSON(w, http.StatusGone, map[string]any{"error": "upload session expired"})
		return
	}

	session = s.syncUploadSessionProgress(r.Context(), session)

	offset, err := parseUploadOffset(r.Header.Get("X-Upload-Offset"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid upload offset"})
		return
	}
	if offset != session.BytesReceived {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":          "upload offset mismatch",
			"expectedOffset": session.BytesReceived,
			"session":        uploadSessionPayloadFromRecord(session),
		})
		return
	}

	remaining := session.FileSize - session.BytesReceived
	if remaining <= 0 {
		writeJSON(w, http.StatusOK, map[string]any{"session": uploadSessionPayloadFromRecord(session)})
		return
	}

	body := http.MaxBytesReader(w, r.Body, remaining)
	written, err := s.storage.AppendChunk(session.StoredPath, offset, body)
	if err != nil {
		s.logger.Printf("append upload chunk: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "append upload chunk"})
		return
	}
	if written == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty upload chunk"})
		return
	}

	now := time.Now().UTC()
	session.BytesReceived += written
	session.UpdatedAt = now
	if session.BytesReceived > session.FileSize {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "upload exceeded declared file size"})
		return
	}

	if session.BytesReceived < session.FileSize {
		if err := s.repo.UpdateUploadSessionProgress(r.Context(), session.ID, session.BytesReceived, now); err != nil {
			s.logger.Printf("update upload session: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "update upload session"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"session": uploadSessionPayloadFromRecord(session)})
		return
	}

	if err := s.storage.FinalizeResumable(session.StoredPath); err != nil {
		s.logger.Printf("finalize upload chunk: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "finalize upload"})
		return
	}

	finalMIMEType, err := s.storage.DetectMIME(session.StoredPath, session.OriginalName, session.MIMEType)
	if err != nil {
		s.logger.Printf("detect upload mime type: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "inspect uploaded file"})
		return
	}

	record := db.FileRecord{
		ID:              session.ID,
		OriginalName:    session.OriginalName,
		StoredPath:      session.StoredPath,
		FileSize:        session.FileSize,
		MIMEType:        finalMIMEType,
		UploadedAt:      now,
		ClientIP:        session.ClientIP,
		ClientHostname:  session.ClientHostname,
		ClientUserAgent: session.ClientUserAgent,
	}
	if err := s.repo.InsertFile(r.Context(), record); err != nil {
		s.logger.Printf("insert resumable file record: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "save upload metadata"})
		return
	}
	if err := s.repo.DeleteUploadSession(r.Context(), session.ID); err != nil {
		s.logger.Printf("delete completed upload session: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "delete upload session"})
		return
	}

	session.MIMEType = finalMIMEType
	session.UpdatedAt = now
	session.CompletedAt = &now
	writeJSON(w, http.StatusOK, map[string]any{
		"session": uploadSessionPayloadFromRecord(session),
		"file": map[string]any{
			"id":           record.ID,
			"originalName": record.OriginalName,
			"fileSize":     record.FileSize,
			"mimeType":     record.MIMEType,
			"uploadedAt":   now.Format(time.RFC3339),
		},
	})
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
		"host": netstate.SnapshotHTTP(s.publicAddr),
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

func uploadSessionPayloadFromRecord(session db.UploadSession) uploadSessionPayload {
	return uploadSessionPayload{
		ID:            session.ID,
		OriginalName:  session.OriginalName,
		FileSize:      session.FileSize,
		MIMEType:      session.MIMEType,
		BytesReceived: session.BytesReceived,
		Complete:      session.CompletedAt != nil,
		CreatedAt:     session.CreatedAt.Format(time.RFC3339),
		UpdatedAt:     session.UpdatedAt.Format(time.RFC3339),
	}
}

func normalizedResumeKey(resumeKey string, originalName string, fileSize int64, lastModified int64) string {
	trimmed := strings.TrimSpace(resumeKey)
	if trimmed != "" {
		return trimmed
	}
	if strings.TrimSpace(originalName) == "" || fileSize <= 0 {
		return ""
	}
	return fmt.Sprintf("%s:%d:%d", strings.TrimSpace(originalName), fileSize, lastModified)
}

func parseUploadOffset(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("parse upload offset: %w", err)
	}
	return parsed, nil
}

func (s *Server) syncUploadSessionProgress(ctx context.Context, session db.UploadSession) db.UploadSession {
	if session.CompletedAt != nil {
		return session
	}

	bytesOnDisk, err := s.storage.PartialSize(session.StoredPath)
	if err != nil || bytesOnDisk == session.BytesReceived {
		return session
	}

	session.BytesReceived = bytesOnDisk
	session.UpdatedAt = time.Now().UTC()
	if err := s.repo.UpdateUploadSessionProgress(ctx, session.ID, session.BytesReceived, session.UpdatedAt); err != nil {
		return session
	}
	return session
}

func (s *Server) isUploadSessionExpired(session db.UploadSession, now time.Time) bool {
	if s.uploadSessionTTL <= 0 || session.CompletedAt != nil {
		return false
	}
	return session.UpdatedAt.Add(s.uploadSessionTTL).Before(now)
}

func (s *Server) abandonUploadSession(ctx context.Context, session db.UploadSession) error {
	if err := s.storage.DeletePartial(session.StoredPath); err != nil {
		return err
	}
	if err := s.repo.DeleteUploadSession(ctx, session.ID); err != nil && !errors.Is(err, db.ErrUploadSessionNotFound) {
		return err
	}
	return nil
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}
