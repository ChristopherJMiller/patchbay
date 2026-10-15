package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPDo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t0k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write([]byte(r.Method + " " + string(b) + "\n"))
	}))
	defer srv.Close()

	code, body, err := httpDo(context.Background(), http.MethodGet, srv.URL, []string{"Authorization: Bearer t0k"}, nil)
	if err != nil || code != http.StatusOK || string(body) != "GET \n" {
		t.Fatalf("with token: %d %q %v", code, body, err)
	}
	code, body, err = httpDo(context.Background(), http.MethodPut, srv.URL, []string{"Authorization: Bearer t0k"}, []byte("firmware"))
	if err != nil || code != http.StatusOK || string(body) != "PUT firmware\n" {
		t.Fatalf("PUT: %d %q %v", code, body, err)
	}
	code, _, err = httpDo(context.Background(), http.MethodGet, srv.URL, nil, nil)
	if err != nil || code != http.StatusUnauthorized {
		t.Fatalf("without token: %d %v", code, err)
	}
	if _, _, err := httpDo(context.Background(), http.MethodGet, srv.URL, []string{"no colon"}, nil); err == nil {
		t.Fatal("a malformed header was accepted")
	}
}

// TestHTTPNeverPrintsAToken: a provisioning URL's token is redacted in
// what fakephone logs, even in a connection error.
func TestHTTPNeverPrintsAToken(t *testing.T) {
	const tok = "s3cr3t-t0k3n"
	if got := redactURL("http://192.0.2.1:8088/p/" + tok); strings.Contains(got, tok) || got != "http://192.0.2.1:8088/p/%3Credacted%3E" {
		t.Errorf("redactURL = %q", got)
	}
	if got := redactURL("http://192.0.2.1:8088/directory.xml"); got != "http://192.0.2.1:8088/directory.xml" {
		t.Errorf("redactURL changed a plain URL: %q", got)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/p/" + tok
	srv.Close() // connection refused from now on
	_, _, err := httpDo(context.Background(), http.MethodGet, url, nil, nil)
	if err == nil || strings.Contains(err.Error(), tok) {
		t.Fatalf("error %v, want one without the token", err)
	}
}
