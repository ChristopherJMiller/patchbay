package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t0k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer srv.Close()

	code, body, err := httpGet(context.Background(), srv.URL, []string{"Authorization: Bearer t0k"})
	if err != nil || code != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("with token: %d %q %v", code, body, err)
	}
	code, _, err = httpGet(context.Background(), srv.URL, nil)
	if err != nil || code != http.StatusUnauthorized {
		t.Fatalf("without token: %d %v", code, err)
	}
	if _, _, err := httpGet(context.Background(), srv.URL, []string{"no colon"}); err == nil {
		t.Fatal("a malformed header was accepted")
	}
}
