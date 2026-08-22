package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGitHubRepoID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/example-owner/feedback-repo" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("Authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1325294260}`))
	}))
	defer srv.Close()

	gh := NewGitHub(srv.URL, time.Second)
	id, err := gh.RepoID(context.Background(), "token", "example-owner/feedback-repo")
	if err != nil {
		t.Fatal(err)
	}
	if id != 1325294260 {
		t.Fatalf("id=%d", id)
	}
}

func TestGitHubRepoIDRejectsMissingID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":0}`))
	}))
	defer srv.Close()

	gh := NewGitHub(srv.URL, time.Second)
	if _, err := gh.RepoID(context.Background(), "token", "example-owner/feedback-repo"); err == nil {
		t.Fatal("GitHub 仓库响应缺少有效 id 时必须失败")
	}
}
