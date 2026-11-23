package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSameOrigin(t *testing.T) {
	for _, test := range []struct {
		name, target, origin, site string
		want                       bool
	}{
		{"matching", "http://127.0.0.1:8081/", "http://127.0.0.1:8081", "same-origin", true},
		{"loopback missing port", "http://127.0.0.1:8081/", "http://127.0.0.1", "same-origin", true},
		{"IPv6 missing port", "http://[::1]:8081/", "http://[::1]", "same-origin", true},
		{"missing fetch metadata", "http://127.0.0.1:8081/", "http://127.0.0.1", "", false},
		{"same site is not same origin", "http://127.0.0.1:8081/", "http://127.0.0.1", "same-site", false},
		{"explicit different port", "http://127.0.0.1:8081/", "http://127.0.0.1:8080", "same-origin", false},
		{"different hostname", "http://127.0.0.1:8081/", "http://localhost", "same-origin", false},
		{"remote missing port", "http://example.com:8081/", "http://example.com", "same-origin", false},
		{"cross site", "http://127.0.0.1:8081/", "http://127.0.0.1", "cross-site", false},
		{"cross site matching origin", "http://127.0.0.1:8081/", "http://127.0.0.1:8081", "cross-site", false},
		{"null origin", "http://127.0.0.1:8081/", "null", "same-origin", false},
		{"origin has path", "http://127.0.0.1:8081/", "http://127.0.0.1/path", "same-origin", false},
		{"origin has credentials", "http://127.0.0.1:8081/", "http://user@127.0.0.1", "same-origin", false},
		{"TLS downgrade", "https://127.0.0.1:8081/", "http://127.0.0.1", "same-origin", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, test.target, nil)
			r.Header.Set("Origin", test.origin)
			r.Header.Set("Sec-Fetch-Site", test.site)
			if got := sameOrigin(r); got != test.want {
				t.Fatalf("sameOrigin() = %v, want %v", got, test.want)
			}
		})
	}
}
