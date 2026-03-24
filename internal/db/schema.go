package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS files (
	id TEXT PRIMARY KEY,
	original_name TEXT NOT NULL,
	stored_path TEXT NOT NULL UNIQUE,
	file_size INTEGER NOT NULL,
	mime_type TEXT NOT NULL,
	uploaded_at TEXT NOT NULL,
	client_ip TEXT NOT NULL,
	client_hostname TEXT,
	client_user_agent TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS upload_sessions (
	id TEXT PRIMARY KEY,
	resume_key TEXT NOT NULL UNIQUE,
	original_name TEXT NOT NULL,
	stored_path TEXT NOT NULL UNIQUE,
	file_size INTEGER NOT NULL,
	mime_type TEXT NOT NULL,
	client_ip TEXT NOT NULL,
	client_hostname TEXT,
	client_user_agent TEXT NOT NULL,
	bytes_received INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	completed_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_files_uploaded_at ON files(uploaded_at DESC);
CREATE INDEX IF NOT EXISTS idx_files_client_ip ON files(client_ip);
CREATE INDEX IF NOT EXISTS idx_upload_sessions_resume_key ON upload_sessions(resume_key);
CREATE INDEX IF NOT EXISTS idx_upload_sessions_completed_at ON upload_sessions(completed_at);
`

type Repository struct {
	db *sql.DB
}

func Open(path string) (*Repository, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// The app only needs light local concurrency, and a single SQLite writer
	// avoids transient "database is locked" failures when multiple uploads start together.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(`PRAGMA busy_timeout = 5000;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set sqlite busy timeout: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return &Repository{db: db}, nil
}

func (r *Repository) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}
