package store

import (
	"context"

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

func (s *Store) ListRequestLogs(ctx context.Context, limit int) ([]*domain.RequestLog, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at, access_key_name, combo, provider, upstream_model, format, stream, status, input_tokens, output_tokens, latency_ms, err_msg
		 FROM request_logs ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
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
