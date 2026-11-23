package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"quickwork-openai/internal/store"
)

type requestRecordKey struct{}

func requestRecord(r *http.Request) *store.RequestRecord {
	row, _ := r.Context().Value(requestRecordKey{}).(*store.RequestRecord)
	return row
}

type recordWriter struct {
	http.ResponseWriter
	row     *store.RequestRecord
	started time.Time
	wrote   bool
}

func (w *recordWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.row.Status = status
	w.row.FirstByteMS = time.Since(w.started).Milliseconds()
	w.ResponseWriter.WriteHeader(status)
}
func (w *recordWriter) Write(data []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.row.Status >= 400 {
		var body struct {
			Error struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &body) == nil {
			w.row.ErrorCode = body.Error.Code
			if w.row.ErrorCode == "" {
				w.row.ErrorCode = body.Error.Type
			}
		}
		w.row.Error = http.StatusText(w.row.Status)
	}
	return w.ResponseWriter.Write(data)
}
func (w *recordWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *recordWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) withRequestRecords(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol := ""
		switch r.URL.Path {
		case "/v1/chat/completions":
			protocol = "chat"
		case "/v1/responses":
			protocol = "responses"
		case "/v1/messages":
			protocol = "messages"
		case "/v1/messages/count_tokens":
			protocol = "count_tokens"
		case "/v1/responses/compact":
			protocol = "compact"
		}
		if protocol == "" || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		row := &store.RequestRecord{ID: "req-" + uuid(), StartedAt: start.UnixMilli(), Protocol: protocol}
		if key := tokenFromRequest(r); key != "" {
			hash := sha256.Sum256([]byte(key))
			row.KeyID = hex.EncodeToString(hash[:6])
		}
		w.Header().Set("X-Request-Id", row.ID)
		r = r.WithContext(context.WithValue(r.Context(), requestRecordKey{}, row))
		writer := &recordWriter{ResponseWriter: w, row: row, started: start}
		next.ServeHTTP(writer, r)
		row.DurationMS = time.Since(start).Milliseconds()
		if row.Status == 0 {
			row.Status = 200
		}
		if r.Context().Err() != nil {
			row.ErrorCode = "request_cancelled"
			row.Error = "Client disconnected or request cancelled"
			if !writer.wrote {
				row.Status = 499
			}
		}
		row.Model = strings.TrimSpace(row.Model)
		if len(row.Model) > 128 {
			row.Model = row.Model[:128]
		}
		if s.accounts != nil {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
			defer cancel()
			if err := s.accounts.RecordRequest(ctx, *row); err != nil {
				log.Printf("request record save failed id=%s", row.ID)
			}
		}
	})
}
