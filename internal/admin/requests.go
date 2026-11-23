package admin

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"quickwork-openai/internal/store"
)

func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset = max(offset, 0)
	rows, total, err := s.store.ListRequests(r.Context(), store.RequestFilter{Protocol: q.Get("protocol"), Model: q.Get("model"), Failed: q.Get("failed") == "true", Limit: limit, Offset: offset})
	if err != nil {
		writeError(w, 500, "records_unavailable", "Could not load request records")
		return
	}
	writeJSON(w, 200, map[string]any{"data": rows, "total": total, "limit": limit, "offset": offset, "retention_days": 30, "max_records": 10000})
}

func (s *Server) handleRequestDetail(w http.ResponseWriter, r *http.Request) {
	row, err := s.store.GetRequest(r.Context(), r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, 404, "not_found", "Request record not found")
		return
	}
	if err != nil {
		writeError(w, 500, "records_unavailable", "Could not load request record")
		return
	}
	writeJSON(w, 200, row)
}
