package store

import (
	"context"
	"encoding/json"
	"time"
)

// RequestRecord deliberately excludes prompts, responses, credentials and image data.
type RequestRecord struct {
	ID               string           `json:"id"`
	StartedAt        int64            `json:"started_at"`
	Protocol         string           `json:"protocol"`
	Model            string           `json:"model"`
	UpstreamModel    string           `json:"upstream_model"`
	Stream           bool             `json:"stream"`
	Status           int              `json:"status"`
	DurationMS       int64            `json:"duration_ms"`
	FirstByteMS      int64            `json:"first_byte_ms,omitempty"`
	AccountID        int64            `json:"account_id,omitempty"`
	Attempts         int              `json:"attempts"`
	PromptTokens     int              `json:"prompt_tokens"`
	CompletionTokens int              `json:"completion_tokens"`
	TotalTokens      int              `json:"total_tokens"`
	CacheReadTokens  int              `json:"cache_read_tokens"`
	CacheWriteTokens int              `json:"cache_write_tokens"`
	CostUSD          *float64         `json:"cost_usd,omitempty"`
	AttemptDetails   []RequestAttempt `json:"attempt_details,omitempty"`
	ToolCalls        int              `json:"tool_calls"`
	Images           int              `json:"images"`
	ContextMode      string           `json:"context_mode,omitempty"`
	KeyID            string           `json:"key_id,omitempty"`
	ErrorCode        string           `json:"error_code,omitempty"`
	Error            string           `json:"error,omitempty"`
}

type RequestAttempt struct {
	AccountID   int64  `json:"account_id"`
	DurationMS  int64  `json:"duration_ms"`
	TotalTokens int    `json:"total_tokens"`
	ErrorCode   string `json:"error_code,omitempty"`
}

type RequestFilter struct {
	Protocol, Model string
	Failed          bool
	Limit, Offset   int
}

func (s *Store) RecordRequest(ctx context.Context, row RequestRecord) error {
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO request_records(id,started_at,protocol,model,status,failed,data) VALUES(?,?,?,?,?,?,?)`, row.ID, row.StartedAt, row.Protocol, row.Model, row.Status, row.Status >= 400 || row.ErrorCode != "", string(data))
	if err != nil {
		return err
	}
	// Bound local history by age and count, even when unauthorized requests arrive.
	_, err = tx.ExecContext(ctx, `DELETE FROM request_records WHERE started_at<? OR rowid NOT IN (SELECT rowid FROM request_records ORDER BY started_at DESC,rowid DESC LIMIT 10000)`, time.Now().Add(-30*24*time.Hour).UnixMilli())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func requestWhere(f RequestFilter) (string, []any) {
	where := " WHERE 1=1"
	args := []any{}
	if f.Protocol != "" {
		where += " AND protocol=?"
		args = append(args, f.Protocol)
	}
	if f.Model != "" {
		where += " AND model=?"
		args = append(args, f.Model)
	}
	if f.Failed {
		where += " AND failed=1"
	}
	return where, args
}

func (s *Store) ListRequests(ctx context.Context, f RequestFilter) ([]RequestRecord, int, error) {
	where, args := requestWhere(f)
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM request_records"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset := max(f.Offset, 0)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, "SELECT data FROM request_records"+where+" ORDER BY started_at DESC,rowid DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := []RequestRecord{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, 0, err
		}
		var row RequestRecord
		if err := json.Unmarshal([]byte(data), &row); err != nil {
			return nil, 0, err
		}
		result = append(result, row)
	}
	return result, total, rows.Err()
}

func (s *Store) GetRequest(ctx context.Context, id string) (RequestRecord, error) {
	var data string
	var row RequestRecord
	if err := s.db.QueryRowContext(ctx, "SELECT data FROM request_records WHERE id=?", id).Scan(&data); err != nil {
		return row, err
	}
	err := json.Unmarshal([]byte(data), &row)
	return row, err
}
