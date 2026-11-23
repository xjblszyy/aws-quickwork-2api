package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRequestRecords(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, row := range []RequestRecord{
		{ID: "req-one", StartedAt: time.Now().UnixMilli(), Protocol: "responses", Model: "opus5.5", UpstreamModel: "smart", Status: 200, Attempts: 2, AccountID: 3, TotalTokens: 42},
		{ID: "req-two", StartedAt: time.Now().UnixMilli(), Protocol: "messages", Status: 401, ErrorCode: "invalid_api_key"},
	} {
		if err := s.RecordRequest(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := s.ListRequests(ctx, RequestFilter{Protocol: "responses", Limit: 20})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Model != "opus5.5" || rows[0].Attempts != 2 {
		t.Fatalf("%+v %d %v", rows, total, err)
	}
	rows, total, err = s.ListRequests(ctx, RequestFilter{Failed: true, Limit: 20})
	if err != nil || total != 1 || rows[0].ID != "req-two" {
		t.Fatalf("failed filter: %+v %d %v", rows, total, err)
	}
	row, err := s.GetRequest(ctx, "req-one")
	if err != nil || row.TotalTokens != 42 {
		t.Fatalf("detail %+v %v", row, err)
	}
	rows, total, err = s.ListRequests(ctx, RequestFilter{Limit: 1, Offset: 1})
	if err != nil || total != 2 || len(rows) != 1 {
		t.Fatalf("page %+v %d %v", rows, total, err)
	}
	if err := s.RecordRequest(ctx, RequestRecord{ID: "expired", StartedAt: time.Now().Add(-31 * 24 * time.Hour).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRequest(ctx, "expired"); err == nil {
		t.Fatal("expired request record was retained")
	}
}
