package store

import (
	"context"
	"fmt"
	"strings"

	"airouter/internal/domain"
)

func (s *Store) CreateRequestLog(ctx context.Context, l *domain.RequestLog) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO request_logs
			(access_key_name, combo, provider, upstream_model, format, stream, status, input_tokens, output_tokens, latency_ms, err_msg)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.AccessKeyName, l.Combo, l.Provider, l.UpstreamModel, l.Format, l.Stream,
		l.Status, l.InputTokens, l.OutputTokens, l.LatencyMS, l.ErrMsg)
	if err != nil {
		return err
	}
	l.ID, err = res.LastInsertId()
	return err
}

// RequestLogQuery filters and pages request logs. Empty string fields are ignored.
// StatusClass is one of: "", "ok" (2xx), "client" (4xx), "server" (5xx), "error" (>=400).
// Page is 1-based; Offset = (Page-1)*Limit.
type RequestLogQuery struct {
	Combo       string
	Provider    string
	StatusClass string
	Page        int
	Limit       int
}

const (
	defaultLogLimit = 20
	maxLogLimit     = 100
)

func clampLogLimit(n int) int {
	if n <= 0 {
		return defaultLogLimit
	}
	if n > maxLogLimit {
		return maxLogLimit
	}
	return n
}

func clampLogPage(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}

// ListRequestLogs returns the newest logs up to limit (legacy helper).
func (s *Store) ListRequestLogs(ctx context.Context, limit int) ([]*domain.RequestLog, error) {
	return s.ListRequestLogsQuery(ctx, RequestLogQuery{Limit: limit, Page: 1})
}

// ListRequestLogsQuery lists logs matching q, newest first, for one page.
func (s *Store) ListRequestLogsQuery(ctx context.Context, q RequestLogQuery) ([]*domain.RequestLog, error) {
	q.Limit = clampLogLimit(q.Limit)
	q.Page = clampLogPage(q.Page)
	where, args := buildLogWhere(q)
	offset := (q.Page - 1) * q.Limit
	sql := `SELECT id, created_at, access_key_name, combo, provider, upstream_model, format, stream, status, input_tokens, output_tokens, latency_ms, err_msg
		 FROM request_logs` + where + ` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, q.Limit, offset)

	rows, err := s.db.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.RequestLog
	for rows.Next() {
		var l domain.RequestLog
		if err := rows.Scan(&l.ID, &l.CreatedAt, &l.AccessKeyName, &l.Combo, &l.Provider,
			&l.UpstreamModel, &l.Format, &l.Stream, &l.Status, &l.InputTokens,
			&l.OutputTokens, &l.LatencyMS, &l.ErrMsg); err != nil {
			return nil, err
		}
		out = append(out, &l)
	}
	return out, rows.Err()
}

// CountRequestLogsQuery returns how many rows match q's filters (ignores Page/Limit).
func (s *Store) CountRequestLogsQuery(ctx context.Context, q RequestLogQuery) (int64, error) {
	where, args := buildLogWhere(q)
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_logs`+where, args...)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func buildLogWhere(q RequestLogQuery) (string, []any) {
	var parts []string
	var args []any
	if c := strings.TrimSpace(q.Combo); c != "" {
		parts = append(parts, "combo = ?")
		args = append(args, c)
	}
	if p := strings.TrimSpace(q.Provider); p != "" {
		parts = append(parts, "provider = ?")
		args = append(args, p)
	}
	switch strings.TrimSpace(q.StatusClass) {
	case "ok":
		parts = append(parts, "status >= 200 AND status < 300")
	case "client":
		parts = append(parts, "status >= 400 AND status < 500")
	case "server":
		parts = append(parts, "status >= 500")
	case "error":
		parts = append(parts, "status >= 400")
	}
	if len(parts) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(parts, " AND "), args
}

// DistinctRequestLogValues returns sorted distinct non-empty values for combo or provider.
func (s *Store) DistinctRequestLogValues(ctx context.Context, column string) ([]string, error) {
	var col string
	switch column {
	case "combo":
		col = "combo"
	case "provider":
		col = "provider"
	default:
		return nil, fmt.Errorf("unsupported log column %q", column)
	}
	// column is fixed from the switch above, not user input.
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT `+col+` FROM request_logs WHERE `+col+` != '' ORDER BY `+col+` LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ClearRequestLogs deletes all recorded request logs.
func (s *Store) ClearRequestLogs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM request_logs")
	return err
}

// RequestLogStats returns aggregate totals across all recorded requests.
func (s *Store) RequestLogStats(ctx context.Context) (totalReqs, totalIn, totalOut int64, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0) FROM request_logs`)
	err = row.Scan(&totalReqs, &totalIn, &totalOut)
	return
}
