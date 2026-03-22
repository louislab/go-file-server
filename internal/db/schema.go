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

CREATE INDEX IF NOT EXISTS idx_files_uploaded_at ON files(uploaded_at DESC);
CREATE INDEX IF NOT EXISTS idx_files_client_ip ON files(client_ip);
`

type Repository struct {
	db *sql.DB
}

func Open(path string) (*Repository, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
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