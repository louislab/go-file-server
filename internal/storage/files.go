package storage

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

type Service struct {
	root string
}

type SaveResult struct {
	StoredPath string
	FileSize   int64
	MIMEType   string
	FileName   string
}

func New(root string) (*Service, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	return &Service{root: absRoot}, nil
}

func (s *Service) Save(id, originalName string, clientContentType string, src io.Reader) (SaveResult, error) {
	if id == "" {
		return SaveResult{}, fmt.Errorf("missing storage id")
	}

	safeName := sanitizeFilename(originalName)
	if safeName == "" {
		safeName = "file"
	}

	relativePath := filepath.ToSlash(filepath.Join(id, safeName))
	absoluteDir := filepath.Join(s.root, id)
	if err := os.MkdirAll(absoluteDir, 0o755); err != nil {
		return SaveResult{}, fmt.Errorf("create file directory: %w", err)
	}

	tmpPath := filepath.Join(absoluteDir, safeName+".part")
	finalPath := filepath.Join(absoluteDir, safeName)

	file, err := os.Create(tmpPath)
	if err != nil {
		return SaveResult{}, fmt.Errorf("create temp file: %w", err)
	}

	var size int64
	var copyErr error
	defer func() {
		_ = file.Close()
		if copyErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	head := make([]byte, 512)
	n, err := src.Read(head)
	if err != nil && err != io.EOF {
		copyErr = fmt.Errorf("read upload stream: %w", err)
		return SaveResult{}, copyErr
	}

	detectedType := "application/octet-stream"
	if n > 0 {
		detectedType = http.DetectContentType(head[:n])
		written, writeErr := file.Write(head[:n])
		if writeErr != nil {
			copyErr = fmt.Errorf("write upload header: %w", writeErr)
			return SaveResult{}, copyErr
		}
		size += int64(written)
	}

	written, err := io.Copy(file, src)
	if err != nil {
		copyErr = fmt.Errorf("stream upload to disk: %w", err)
		return SaveResult{}, copyErr
	}
	size += written

	if err := file.Close(); err != nil {
		copyErr = fmt.Errorf("close upload file: %w", err)
		return SaveResult{}, copyErr
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		copyErr = fmt.Errorf("finalize upload file: %w", err)
		return SaveResult{}, copyErr
	}

	mimeType := chooseMIMEType(clientContentType, detectedType, originalName)

	copyErr = nil
	return SaveResult{
		StoredPath: relativePath,
		FileSize:   size,
		MIMEType:   mimeType,
		FileName:   safeName,
	}, nil
}

func (s *Service) Open(storedPath string) (*os.File, string, error) {
	absolutePath, err := s.absolutePath(storedPath)
	if err != nil {
		return nil, "", err
	}

	file, err := os.Open(absolutePath)
	if err != nil {
		return nil, "", fmt.Errorf("open file: %w", err)
	}

	return file, absolutePath, nil
}

func (s *Service) Delete(storedPath string) error {
	absolutePath, err := s.absolutePath(storedPath)
	if err != nil {
		return err
	}

	if err := os.Remove(absolutePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove file: %w", err)
	}

	parent := filepath.Dir(absolutePath)
	if sameDir(parent, s.root) {
		return nil
	}

	if err := os.Remove(parent); err != nil && !os.IsNotExist(err) {
		if !strings.Contains(err.Error(), "directory not empty") {
			return fmt.Errorf("remove upload directory: %w", err)
		}
	}

	return nil
}

func sanitizeFilename(name string) string {
	cleaned := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	cleaned = strings.TrimSpace(cleaned)
	cleaned = strings.ReplaceAll(cleaned, "..", "")

	var builder strings.Builder
	for _, char := range cleaned {
		switch {
		case unicode.IsLetter(char), unicode.IsDigit(char):
			builder.WriteRune(char)
		case strings.ContainsRune(".-_() ", char):
			builder.WriteRune(char)
		default:
			builder.WriteRune('_')
		}
	}

	result := strings.Trim(builder.String(), " .")
	if len(result) > 255 {
		result = result[:255]
	}
	return result
}

func sameDir(left string, right string) bool {
	leftClean := filepath.Clean(left)
	rightClean := filepath.Clean(right)
	return leftClean == rightClean
}

func chooseMIMEType(clientContentType string, detectedType string, originalName string) string {
	clientType := normalizeMIMEType(clientContentType)
	if clientType != "" && clientType != "application/octet-stream" {
		return clientType
	}

	detectedType = normalizeMIMEType(detectedType)
	if detectedType != "" && detectedType != "application/octet-stream" {
		return detectedType
	}

	if guessedType := normalizeMIMEType(mime.TypeByExtension(strings.ToLower(filepath.Ext(originalName)))); guessedType != "" {
		return guessedType
	}

	if clientType != "" {
		return clientType
	}
	if detectedType != "" {
		return detectedType
	}
	return "application/octet-stream"
}

func normalizeMIMEType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err == nil {
		return strings.ToLower(mediaType)
	}
	return strings.ToLower(value)
}

func (s *Service) absolutePath(storedPath string) (string, error) {
	cleanRelative := filepath.Clean(filepath.FromSlash(storedPath))
	absolutePath := filepath.Join(s.root, cleanRelative)
	absolutePath = filepath.Clean(absolutePath)

	rootWithSeparator := s.root + string(os.PathSeparator)
	if absolutePath != s.root && !strings.HasPrefix(absolutePath, rootWithSeparator) {
		return "", fmt.Errorf("invalid stored path")
	}

	return absolutePath, nil
}
