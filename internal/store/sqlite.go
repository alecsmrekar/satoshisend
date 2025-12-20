package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var ErrNotFound = errors.New("not found")

// SQLiteStore implements Store using SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates a new SQLite-backed store.
func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		return nil, err
	}

	if err := migrate(db); err != nil {
		return nil, err
	}

	return &SQLiteStore{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS files (
			id TEXT PRIMARY KEY,
			size INTEGER NOT NULL,
			expires_at DATETIME NOT NULL,
			host_duration_ns INTEGER NOT NULL DEFAULT 0,
			paid INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL
		)
	`)
	if err != nil {
		return err
	}

	// Add host_duration_ns column if it doesn't exist (migration for existing DBs)
	_, _ = db.Exec(`ALTER TABLE files ADD COLUMN host_duration_ns INTEGER NOT NULL DEFAULT 0`)

	return nil
}

func (s *SQLiteStore) SaveFileMetadata(ctx context.Context, meta *FileMeta) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO files (id, size, expires_at, host_duration_ns, paid, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, meta.ID, meta.Size, meta.ExpiresAt, int64(meta.HostDuration), meta.Paid, meta.CreatedAt)
	return err
}

func (s *SQLiteStore) GetFileMetadata(ctx context.Context, id string) (*FileMeta, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, size, expires_at, host_duration_ns, paid, created_at
		FROM files WHERE id = ?
	`, id)

	var meta FileMeta
	var hostDurationNs int64
	var paid int
	err := row.Scan(&meta.ID, &meta.Size, &meta.ExpiresAt, &hostDurationNs, &paid, &meta.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	meta.HostDuration = time.Duration(hostDurationNs)
	meta.Paid = paid == 1
	return &meta, nil
}

func (s *SQLiteStore) UpdatePaymentStatus(ctx context.Context, fileID string, paid bool) error {
	paidInt := 0
	if paid {
		paidInt = 1
	}

	var result sql.Result
	var err error

	if paid {
		// When marking as paid, extend expiration based on stored host_duration
		result, err = s.db.ExecContext(ctx, `
			UPDATE files
			SET paid = ?, expires_at = datetime('now', '+' || (host_duration_ns / 1000000000) || ' seconds')
			WHERE id = ?
		`, paidInt, fileID)
	} else {
		result, err = s.db.ExecContext(ctx, `
			UPDATE files SET paid = ? WHERE id = ?
		`, paidInt, fileID)
	}

	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) DeleteFileMetadata(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) ListExpiredFiles(ctx context.Context) ([]*FileMeta, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, size, expires_at, host_duration_ns, paid, created_at
		FROM files WHERE expires_at < ?
	`, time.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []*FileMeta
	for rows.Next() {
		var meta FileMeta
		var hostDurationNs int64
		var paid int
		if err := rows.Scan(&meta.ID, &meta.Size, &meta.ExpiresAt, &hostDurationNs, &paid, &meta.CreatedAt); err != nil {
			return nil, err
		}
		meta.HostDuration = time.Duration(hostDurationNs)
		meta.Paid = paid == 1
		files = append(files, &meta)
	}
	return files, rows.Err()
}

func (s *SQLiteStore) GetStats(ctx context.Context) (*Stats, error) {
	stats := &Stats{}

	// Get counts and sizes
	row := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*) as total,
			COALESCE(SUM(CASE WHEN paid = 1 THEN 1 ELSE 0 END), 0) as paid_count,
			COALESCE(SUM(CASE WHEN paid = 0 THEN 1 ELSE 0 END), 0) as pending_count,
			COALESCE(SUM(CASE WHEN expires_at < datetime('now') THEN 1 ELSE 0 END), 0) as expired_count,
			COALESCE(SUM(size), 0) as total_bytes,
			COALESCE(SUM(CASE WHEN paid = 1 THEN size ELSE 0 END), 0) as paid_bytes,
			COALESCE(SUM(CASE WHEN paid = 0 THEN size ELSE 0 END), 0) as pending_bytes,
			COALESCE(MIN(created_at), '') as oldest,
			COALESCE(MAX(created_at), '') as newest
		FROM files
	`)

	var oldest, newest string
	err := row.Scan(
		&stats.TotalFiles,
		&stats.PaidFiles,
		&stats.PendingFiles,
		&stats.ExpiredFiles,
		&stats.TotalBytes,
		&stats.PaidBytes,
		&stats.PendingBytes,
		&oldest,
		&newest,
	)
	if err != nil {
		return nil, err
	}

	if oldest != "" {
		stats.OldestFile, _ = time.Parse("2006-01-02 15:04:05-07:00", oldest)
		if stats.OldestFile.IsZero() {
			stats.OldestFile, _ = time.Parse("2006-01-02T15:04:05Z", oldest)
		}
	}
	if newest != "" {
		stats.NewestFile, _ = time.Parse("2006-01-02 15:04:05-07:00", newest)
		if stats.NewestFile.IsZero() {
			stats.NewestFile, _ = time.Parse("2006-01-02T15:04:05Z", newest)
		}
	}

	return stats, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
