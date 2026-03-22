package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type FileRecord struct {
	ID             string    `json:"id"`
	OriginalName   string    `json:"originalName"`
	StoredPath     string    `json:"-"`
	FileSize       int64     `json:"fileSize"`
	MIMEType       string    `json:"mimeType"`
	UploadedAt     time.Time `json:"uploadedAt"`
	ClientIP       string    `json:"clientIP"`
	ClientHostname string    `json:"clientHostname,omitempty"`
	ClientUserAgent string   `json:"clientUserAgent,omitempty"`
}

func (r *Repository) InsertFile(ctx context.Context, record FileRecord) error {
	_, err := r.db.ExecContext(
		ctx,
		`INSERT INTO files (
			id,
			original_name,
			stored_path,
			file_size,
			mime_type,
			uploaded_at,
			client_ip,
			client_hostname,
			client_user_agent
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID,
		record.OriginalName,
		record.StoredPath,
		record.FileSize,
		record.MIMEType,
		record.UploadedAt.UTC().Format(time.RFC3339Nano),
		record.ClientIP,
		nullableString(record.ClientHostname),
		record.ClientUserAgent,
	)
	if err != nil {
		return fmt.Errorf("insert file: %w", err)
	}
	return nil
}

func (r *Repository) ListFiles(ctx context.Context) ([]FileRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, original_name, stored_path, file_size, mime_type, uploaded_at, client_ip, client_hostname, client_user_agent
		FROM files
		ORDER BY uploaded_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()

	var records []FileRecord
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate files: %w", err)
	}

	if records == nil {
		return []FileRecord{}, nil
	}
	return records, nil
}

func (r *Repository) GetFile(ctx context.Context, id string) (FileRecord, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, original_name, stored_path, file_size, mime_type, uploaded_at, client_ip, client_hostname, client_user_agent
		FROM files
		WHERE id = ?`, id)

	record, err := scanRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FileRecord{}, ErrNotFound
		}
		return FileRecord{}, err
	}

	return record, nil
}

func (r *Repository) DeleteFile(ctx context.Context, id string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete file: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete file rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrNotFound
	}

	return nil
}

var ErrNotFound = errors.New("file not found")

type scanner interface {
	Scan(dest ...any) error
}

func scanRecord(scan scanner) (FileRecord, error) {
	var record FileRecord
	var uploadedAt string
	var hostname sql.NullString

	if err := scan.Scan(
		&record.ID,
		&record.OriginalName,
		&record.StoredPath,
		&record.FileSize,
		&record.MIMEType,
		&uploadedAt,
		&record.ClientIP,
		&hostname,
		&record.ClientUserAgent,
	); err != nil {
		return FileRecord{}, err
	}

	parsed, err := time.Parse(time.RFC3339Nano, uploadedAt)
	if err != nil {
		return FileRecord{}, fmt.Errorf("parse uploaded_at: %w", err)
	}
	record.UploadedAt = parsed
	record.ClientHostname = hostname.String

	return record, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}