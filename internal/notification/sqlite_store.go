package notification

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z"

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, errors.New("sqlite database path is required")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create sqlite dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)

	store := &SQLiteStore{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) init() error {
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS notifications (
			id TEXT PRIMARY KEY,
			target_url TEXT NOT NULL,
			method TEXT NOT NULL,
			headers_json TEXT NOT NULL,
			body_json BLOB NOT NULL,
			idempotency_key TEXT,
			status TEXT NOT NULL,
			attempt_count INTEGER NOT NULL,
			max_attempts INTEGER NOT NULL,
			next_retry_at TEXT,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS notifications_idem_key_uq
			ON notifications(idempotency_key)
			WHERE idempotency_key IS NOT NULL AND idempotency_key <> ''`,
		`CREATE INDEX IF NOT EXISTS notifications_due_idx
			ON notifications(status, next_retry_at, updated_at, created_at)`,
		`CREATE TABLE IF NOT EXISTS delivery_attempts (
			id TEXT PRIMARY KEY,
			notification_id TEXT NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
			attempt_no INTEGER NOT NULL,
			status TEXT NOT NULL,
			status_code INTEGER NOT NULL DEFAULT 0,
			retryable INTEGER NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			next_retry_at TEXT,
			started_at TEXT NOT NULL,
			finished_at TEXT NOT NULL,
			UNIQUE(notification_id, attempt_no)
		)`,
		`CREATE INDEX IF NOT EXISTS delivery_attempts_notification_idx
			ON delivery_attempts(notification_id, attempt_no)`,
	}
	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("initialize sqlite store: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) Create(n Notification) (Notification, bool, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return Notification{}, false, err
	}
	defer tx.Rollback()

	if err := insertNotification(tx, n); err != nil {
		if n.IdempotencyKey != "" {
			existing, findErr := getNotificationByIdempotencyKey(tx, n.IdempotencyKey)
			if findErr == nil {
				if !sameIdempotentRequest(existing, n) {
					return Notification{}, false, ErrIdempotencyConflict
				}
				return existing, true, tx.Commit()
			}
			if !errors.Is(findErr, ErrNotFound) {
				return Notification{}, false, findErr
			}
		}
		return Notification{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Notification{}, false, err
	}
	return cloneNotification(n), false, nil
}

func (s *SQLiteStore) Get(id string) (Notification, error) {
	return getNotification(s.db, `WHERE id = ?`, id)
}

func (s *SQLiteStore) ListByStatus(status Status) ([]Notification, error) {
	if _, err := ParseStatus(string(status)); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(notificationSelectSQL()+` WHERE status = ? ORDER BY created_at, id`, string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNotifications(rows)
}

func (s *SQLiteStore) ClaimDue(now time.Time, limit int, sendingLease time.Duration) ([]Notification, error) {
	if limit <= 0 {
		return nil, errors.New("limit must be positive")
	}
	nowText := formatSQLiteTime(now)
	leaseExpiredText := formatSQLiteTime(now.Add(-sendingLease))

	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT id
		FROM notifications
		WHERE status = ?
			OR (status = ? AND (next_retry_at IS NULL OR next_retry_at <= ?))
			OR (status = ? AND updated_at <= ?)
		ORDER BY created_at, id
		LIMIT ?`,
		string(StatusPending),
		string(StatusRetrying), nowText,
		string(StatusSending), leaseExpiredText,
		limit,
	)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	claimed := make([]Notification, 0, len(ids))
	for _, id := range ids {
		result, err := tx.Exec(`
			UPDATE notifications
			SET status = ?, attempt_count = attempt_count + 1, next_retry_at = NULL, last_error = '', updated_at = ?
			WHERE id = ? AND (
				status = ?
				OR (status = ? AND (next_retry_at IS NULL OR next_retry_at <= ?))
				OR (status = ? AND updated_at <= ?)
			)`,
			string(StatusSending), nowText,
			id,
			string(StatusPending),
			string(StatusRetrying), nowText,
			string(StatusSending), leaseExpiredText,
		)
		if err != nil {
			return nil, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if affected == 0 {
			continue
		}
		n, err := getNotification(tx, `WHERE id = ?`, id)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, n)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

func (s *SQLiteStore) MarkSuccess(id string, now time.Time) error {
	return updateNotificationStatus(s.db, id, StatusSucceeded, "", nil, now)
}

func (s *SQLiteStore) MarkRetry(id string, lastError string, nextRetryAt time.Time, now time.Time) error {
	return updateNotificationStatus(s.db, id, StatusRetrying, lastError, &nextRetryAt, now)
}

func (s *SQLiteStore) MarkFailed(id string, lastError string, now time.Time) error {
	return updateNotificationStatus(s.db, id, StatusFailed, lastError, nil, now)
}

func (s *SQLiteStore) RetryFailed(id string, now time.Time) (Notification, error) {
	result, err := s.db.Exec(`
		UPDATE notifications
		SET status = ?, next_retry_at = NULL, last_error = '', updated_at = ?
		WHERE id = ? AND status = ?`,
		string(StatusRetrying), formatSQLiteTime(now), id, string(StatusFailed),
	)
	if err != nil {
		return Notification{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Notification{}, err
	}
	if affected == 0 {
		n, getErr := s.Get(id)
		if errors.Is(getErr, ErrNotFound) {
			return Notification{}, ErrNotFound
		}
		if getErr != nil {
			return Notification{}, getErr
		}
		return Notification{}, fmt.Errorf("notification %s is %s and cannot be retried manually", id, n.Status)
	}
	return s.Get(id)
}

func (s *SQLiteStore) RecordAttempt(attempt DeliveryAttempt) error {
	if err := validateAttempt(attempt); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		INSERT INTO delivery_attempts (
			id, notification_id, attempt_no, status, status_code,
			retryable, error, next_retry_at, started_at, finished_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attempt.ID,
		attempt.NotificationID,
		attempt.AttemptNo,
		string(attempt.Status),
		attempt.StatusCode,
		boolToInt(attempt.Retryable),
		attempt.Error,
		formatNullableSQLiteTime(attempt.NextRetryAt),
		formatSQLiteTime(attempt.StartedAt),
		formatSQLiteTime(attempt.FinishedAt),
	)
	if err != nil {
		return fmt.Errorf("record delivery attempt: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListAttempts(notificationID string) ([]DeliveryAttempt, error) {
	if _, err := s.Get(notificationID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT id, notification_id, attempt_no, status, status_code,
			retryable, error, next_retry_at, started_at, finished_at
		FROM delivery_attempts
		WHERE notification_id = ?
		ORDER BY attempt_no, started_at`,
		notificationID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	attempts := make([]DeliveryAttempt, 0)
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return attempts, nil
}

func insertNotification(tx *sql.Tx, n Notification) error {
	headers, err := json.Marshal(n.Headers)
	if err != nil {
		return fmt.Errorf("encode headers: %w", err)
	}
	_, err = tx.Exec(`
		INSERT INTO notifications (
			id, target_url, method, headers_json, body_json, idempotency_key,
			status, attempt_count, max_attempts, next_retry_at, last_error,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID,
		n.TargetURL,
		n.Method,
		string(headers),
		[]byte(n.Body),
		nullableString(n.IdempotencyKey),
		string(n.Status),
		n.AttemptCount,
		n.MaxAttempts,
		formatNullableSQLiteTime(n.NextRetryAt),
		n.LastError,
		formatSQLiteTime(n.CreatedAt),
		formatSQLiteTime(n.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert notification: %w", err)
	}
	return nil
}

func updateNotificationStatus(db *sql.DB, id string, status Status, lastError string, nextRetryAt *time.Time, now time.Time) error {
	result, err := db.Exec(`
		UPDATE notifications
		SET status = ?, next_retry_at = ?, last_error = ?, updated_at = ?
		WHERE id = ?`,
		string(status),
		formatNullableSQLiteTime(nextRetryAt),
		lastError,
		formatSQLiteTime(now),
		id,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func getNotificationByIdempotencyKey(tx *sql.Tx, key string) (Notification, error) {
	return getNotification(tx, `WHERE idempotency_key = ?`, key)
}

type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func getNotification(q queryer, where string, args ...any) (Notification, error) {
	row := q.QueryRow(notificationSelectSQL()+" "+where, args...)
	n, err := scanNotification(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, ErrNotFound
	}
	if err != nil {
		return Notification{}, err
	}
	return n, nil
}

func notificationSelectSQL() string {
	return `SELECT id, target_url, method, headers_json, body_json, idempotency_key,
		status, attempt_count, max_attempts, next_retry_at, last_error, created_at, updated_at
		FROM notifications`
}

type scanner interface {
	Scan(dest ...any) error
}

func scanNotification(row scanner) (Notification, error) {
	var n Notification
	var headersRaw string
	var body []byte
	var idempotencyKey sql.NullString
	var status string
	var nextRetryAt sql.NullString
	var createdAt string
	var updatedAt string
	if err := row.Scan(
		&n.ID,
		&n.TargetURL,
		&n.Method,
		&headersRaw,
		&body,
		&idempotencyKey,
		&status,
		&n.AttemptCount,
		&n.MaxAttempts,
		&nextRetryAt,
		&n.LastError,
		&createdAt,
		&updatedAt,
	); err != nil {
		return Notification{}, err
	}
	headers := map[string]string{}
	if err := json.Unmarshal([]byte(headersRaw), &headers); err != nil {
		return Notification{}, fmt.Errorf("decode headers for notification %s: %w", n.ID, err)
	}
	parsedStatus, err := ParseStatus(status)
	if err != nil {
		return Notification{}, err
	}
	created, err := parseSQLiteTime(createdAt)
	if err != nil {
		return Notification{}, fmt.Errorf("decode created_at for notification %s: %w", n.ID, err)
	}
	updated, err := parseSQLiteTime(updatedAt)
	if err != nil {
		return Notification{}, fmt.Errorf("decode updated_at for notification %s: %w", n.ID, err)
	}
	next, err := parseNullableSQLiteTime(nextRetryAt)
	if err != nil {
		return Notification{}, fmt.Errorf("decode next_retry_at for notification %s: %w", n.ID, err)
	}
	n.Headers = headers
	n.Body = append(json.RawMessage(nil), body...)
	if idempotencyKey.Valid {
		n.IdempotencyKey = idempotencyKey.String
	}
	n.Status = parsedStatus
	n.NextRetryAt = next
	n.CreatedAt = created
	n.UpdatedAt = updated
	return n, nil
}

func scanNotifications(rows *sql.Rows) ([]Notification, error) {
	items := make([]Notification, 0)
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func scanAttempt(row scanner) (DeliveryAttempt, error) {
	var attempt DeliveryAttempt
	var status string
	var retryable int
	var nextRetryAt sql.NullString
	var startedAt string
	var finishedAt string
	if err := row.Scan(
		&attempt.ID,
		&attempt.NotificationID,
		&attempt.AttemptNo,
		&status,
		&attempt.StatusCode,
		&retryable,
		&attempt.Error,
		&nextRetryAt,
		&startedAt,
		&finishedAt,
	); err != nil {
		return DeliveryAttempt{}, err
	}
	switch AttemptStatus(status) {
	case AttemptSucceeded, AttemptRetrying, AttemptFailed:
		attempt.Status = AttemptStatus(status)
	default:
		return DeliveryAttempt{}, fmt.Errorf("unknown attempt status %q", status)
	}
	next, err := parseNullableSQLiteTime(nextRetryAt)
	if err != nil {
		return DeliveryAttempt{}, fmt.Errorf("decode next_retry_at for attempt %s: %w", attempt.ID, err)
	}
	started, err := parseSQLiteTime(startedAt)
	if err != nil {
		return DeliveryAttempt{}, fmt.Errorf("decode started_at for attempt %s: %w", attempt.ID, err)
	}
	finished, err := parseSQLiteTime(finishedAt)
	if err != nil {
		return DeliveryAttempt{}, fmt.Errorf("decode finished_at for attempt %s: %w", attempt.ID, err)
	}
	attempt.Retryable = retryable != 0
	attempt.NextRetryAt = next
	attempt.StartedAt = started
	attempt.FinishedAt = finished
	return attempt, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func formatSQLiteTime(t time.Time) string {
	return t.UTC().Format(sqliteTimeLayout)
}

func formatNullableSQLiteTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	formatted := formatSQLiteTime(*t)
	return formatted
}

func parseSQLiteTime(value string) (time.Time, error) {
	return time.Parse(sqliteTimeLayout, value)
}

func parseNullableSQLiteTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseSQLiteTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
