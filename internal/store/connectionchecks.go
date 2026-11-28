package store

import (
	"context"
	"fmt"
	"time"
)

// ConnectionCheck is what the System health page shows for one connection:
// when it last worked and the code of its last error. Kind is "aws", "github"
// or "slack"; Ref is the AWS account id, the GitHub owner or "slack".
type ConnectionCheck struct {
	Kind, Ref     string
	LastOKAt      *time.Time
	LastErrorAt   *time.Time
	LastErrorCode string
}

// RecordConnectionOK sets when the connection last worked. The last error stays.
func RecordConnectionOK(ctx context.Context, db DBTX, kind, ref string, at time.Time) error {
	_, err := db.Exec(ctx, `
		INSERT INTO connection_checks (kind, ref, last_ok_at) VALUES ($1, $2, $3)
		ON CONFLICT (kind, ref) DO UPDATE SET last_ok_at = EXCLUDED.last_ok_at`,
		kind, ref, at)
	if err != nil {
		return fmt.Errorf("record connection ok: %w", err)
	}
	return nil
}

// RecordConnectionError sets when the connection last failed and its error
// code. The last time it worked stays.
func RecordConnectionError(ctx context.Context, db DBTX, kind, ref, code string, at time.Time) error {
	_, err := db.Exec(ctx, `
		INSERT INTO connection_checks (kind, ref, last_error_at, last_error_code) VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, ref) DO UPDATE SET last_error_at = EXCLUDED.last_error_at, last_error_code = EXCLUDED.last_error_code`,
		kind, ref, at, code)
	if err != nil {
		return fmt.Errorf("record connection error: %w", err)
	}
	return nil
}

// ListConnectionChecks returns every recorded check ordered by kind and ref.
// The result is never nil.
func ListConnectionChecks(ctx context.Context, db DBTX) ([]ConnectionCheck, error) {
	rows, err := db.Query(ctx, `
		SELECT kind, ref, last_ok_at, last_error_at, last_error_code
		FROM connection_checks ORDER BY kind, ref`)
	if err != nil {
		return nil, fmt.Errorf("list connection checks: %w", err)
	}
	defer rows.Close()
	checks := []ConnectionCheck{}
	for rows.Next() {
		var c ConnectionCheck
		if err := rows.Scan(&c.Kind, &c.Ref, &c.LastOKAt, &c.LastErrorAt, &c.LastErrorCode); err != nil {
			return nil, fmt.Errorf("scan connection check: %w", err)
		}
		checks = append(checks, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list connection checks: %w", err)
	}
	return checks, nil
}
