package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/store"
)

func TestRequestRecordsRequireManagementSession(t *testing.T) {
	t.Setenv("QUICKPROXY_ADMIN_PASSWORD", "test-management-password")
	cfg := config.Default()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	server, err := NewServer(&cfg, st, accounts.New(st, &cfg), false)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	handler := server.Handler(http.NotFoundHandler())
	if err := st.RecordRequest(context.Background(), store.RequestRecord{ID: "request-one", StartedAt: time.Now().UnixMilli(), Protocol: "messages", Status: 200, Model: "opus5.5"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/requests", "/api/requests/request-one"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer inference-key")
		handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("management route accepted inference key: %d", w.Code)
		}
	}
	login := httptest.NewRequest("POST", "http://console.local/api/auth/login", strings.NewReader(`{"password":"test-management-password"}`))
	login.Header.Set("Origin", "http://console.local")
	login.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, login)
	if w.Code != 200 {
		t.Fatalf("login %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no management session")
	}
	for _, path := range []string{"/api/requests?protocol=messages", "/api/requests/request-one"} {
		r := httptest.NewRequest("GET", path, nil)
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "opus5.5") {
			t.Fatalf("%s %d %s", path, w.Code, w.Body.String())
		}
	}
}
