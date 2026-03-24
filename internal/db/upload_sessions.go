package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type UploadSession struct {
	ID              string     `json:"id"`
	ResumeKey       string     `json:"-"`
	OriginalName    string     `json:"originalName"`
	StoredPath      string     `json:"-"`
	FileSize        int64      `json:"fileSize"`
	MIMEType        string     `json:"mimeType"`
	ClientIP        string     `json:"-"`
	ClientHostname  string     `json:"-"`
	ClientUserAgent string     `json:"-"`
	BytesReceived   int64      `json:"bytesReceived"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	CompletedAt     *time.Time `json:"completedAt,omitempty"`
}

var ErrUploadSessionNotFound = errors.New("upload session not found")

func (r *Repository) CreateUploadSession(ctx context.Context, session UploadSession) error {
	_, err := r.db.ExecContext(
		ctx,
		`INSERT INTO upload_sessions (
			id,
			resume_key,
			original_name,
			stored_path,
			file_size,
			mime_type,
			client_ip,
			client_hostname,
			client_user_agent,
			bytes_received,
			created_at,
			updated_at,
			completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID,
		session.ResumeKey,
		session.OriginalName,
		session.StoredPath,
		session.FileSize,
		session.MIMEType,
		session.ClientIP,
		nullableString(session.ClientHostname),
		session.ClientUserAgent,
		session.BytesReceived,
		session.CreatedAt.UTC().Format(time.RFC3339Nano),
		session.UpdatedAt.UTC().Format(time.RFC3339Nano),
		nullableTime(session.CompletedAt),
	)
	if err != nil {
		return fmt.Errorf("insert upload session: %w", err)
	}
	return nil
}

func (r *Repository) FindActiveUploadSessionByResumeKey(ctx context.Context, resumeKey string) (UploadSession, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, resume_key, original_name, stored_path, file_size, mime_type, client_ip, client_hostname, client_user_agent, bytes_received, created_at, updated_at, completed_at
		FROM upload_sessions
		WHERE resume_key = ? AND completed_at IS NULL
		LIMIT 1`, resumeKey)

	session, err := scanUploadSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UploadSession{}, ErrUploadSessionNotFound
		}
		return UploadSession{}, err
	}

	return session, nil
}

func (r *Repository) GetUploadSession(ctx context.Context, id string) (UploadSession, error) {
	row := r.db.QueryRowContext(ctx, `
		SELECT id, resume_key, original_name, stored_path, file_size, mime_type, client_ip, client_hostname, client_user_agent, bytes_received, created_at, updated_at, completed_at
		FROM upload_sessions
		WHERE id = ?`, id)

	session, err := scanUploadSession(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UploadSession{}, ErrUploadSessionNotFound
		}
		return UploadSession{}, err
	}

	return session, nil
}

func (r *Repository) ListStaleUploadSessions(ctx context.Context, cutoff time.Time) ([]UploadSession, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, resume_key, original_name, stored_path, file_size, mime_type, client_ip, client_hostname, client_user_agent, bytes_received, created_at, updated_at, completed_at
		FROM upload_sessions
		WHERE completed_at IS NULL AND updated_at < ?
		ORDER BY updated_at ASC`, cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("list stale upload sessions: %w", err)
	}
	defer rows.Close()

	sessions := make([]UploadSession, 0)
	for rows.Next() {
		session, err := scanUploadSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stale upload sessions: %w", err)
	}

	return sessions, nil
}

func (r *Repository) DeleteUploadSession(ctx context.Context, id string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM upload_sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete upload session: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete upload session rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrUploadSessionNotFound
	}

	return nil
}

func (r *Repository) UpdateUploadSessionProgress(ctx context.Context, id string, bytesReceived int64, updatedAt time.Time) error {
	result, err := r.db.ExecContext(
		ctx,
		`UPDATE upload_sessions
		SET bytes_received = ?, updated_at = ?
		WHERE id = ? AND completed_at IS NULL`,
		bytesReceived,
		updatedAt.UTC().Format(time.RFC3339Nano),
		id,
	)
	if err != nil {
		return fmt.Errorf("update upload session progress: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update upload session progress rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrUploadSessionNotFound
	}

	return nil
}

func (r *Repository) CompleteUploadSession(ctx context.Context, id string, bytesReceived int64, mimeType string, completedAt time.Time) error {
	result, err := r.db.ExecContext(
		ctx,
		`UPDATE upload_sessions
		SET bytes_received = ?, mime_type = ?, updated_at = ?, completed_at = ?
		WHERE id = ? AND completed_at IS NULL`,
		bytesReceived,
		mimeType,
		completedAt.UTC().Format(time.RFC3339Nano),
		completedAt.UTC().Format(time.RFC3339Nano),
		id,
	)
	if err != nil {
		return fmt.Errorf("complete upload session: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete upload session rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrUploadSessionNotFound
	}

	return nil
}

func scanUploadSession(scan scanner) (UploadSession, error) {
	var session UploadSession
	var createdAt string
	var updatedAt string
	var hostname sql.NullString
	var completedAt sql.NullString

	if err := scan.Scan(
		&session.ID,
		&session.ResumeKey,
		&session.OriginalName,
		&session.StoredPath,
		&session.FileSize,
		&session.MIMEType,
		&session.ClientIP,
		&hostname,
		&session.ClientUserAgent,
		&session.BytesReceived,
		&createdAt,
		&updatedAt,
		&completedAt,
	); err != nil {
		return UploadSession{}, err
	}

	parsedCreatedAt, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return UploadSession{}, fmt.Errorf("parse upload session created_at: %w", err)
	}
	parsedUpdatedAt, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return UploadSession{}, fmt.Errorf("parse upload session updated_at: %w", err)
	}

	session.CreatedAt = parsedCreatedAt
	session.UpdatedAt = parsedUpdatedAt
	session.ClientHostname = hostname.String

	if completedAt.Valid && strings.TrimSpace(completedAt.String) != "" {
		parsedCompletedAt, err := time.Parse(time.RFC3339Nano, completedAt.String)
		if err != nil {
			return UploadSession{}, fmt.Errorf("parse upload session completed_at: %w", err)
		}
		session.CompletedAt = &parsedCompletedAt
	}

	return session, nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func isUploadSessionResumeKeyConstraint(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint failed") && strings.Contains(message, "upload_sessions.resume_key")
}
