package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfigForTest(t *testing.T, raw map[string]any) string {
	t.Helper()
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigAttachmentCredentialsMustBePaired(t *testing.T) {
	tests := []map[string]any{
		{"githubAttachmentSessionFile": "/tmp/session"},
		{"githubAttachmentAccount": "attachment-bot"},
	}
	for i, raw := range tests {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			_, err := LoadConfig(writeConfigForTest(t, raw))
			if err == nil || !strings.Contains(err.Error(), "必须同时配置") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestLoadConfigAcceptsPairedAttachmentCredentials(t *testing.T) {
	path := writeConfigForTest(t, map[string]any{
		"githubAttachmentSessionFile": " /opt/repomcp/secrets/github-attachment-session ",
		"githubAttachmentAccount":     " attachment-bot ",
		"repos": []map[string]any{{
			"name": "feedback",
			"url":  "https://github.com/example-owner/example-repo.git",
		}},
	})
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubAttachmentSessionFile != "/opt/repomcp/secrets/github-attachment-session" {
		t.Fatalf("session file=%q", cfg.GitHubAttachmentSessionFile)
	}
	if cfg.GitHubAttachmentAccount != "attachment-bot" {
		t.Fatalf("account=%q", cfg.GitHubAttachmentAccount)
	}
}

func TestLoadConfigRejectsRetiredServerMediaFields(t *testing.T) {
	for _, field := range []string{"mediaStoreDir", "mediaPublicBaseURL"} {
		t.Run(field, func(t *testing.T) {
			raw := map[string]any{
				field: "/retired",
				"repos": []map[string]any{{
					"name": "feedback",
					"url":  "https://github.com/example-owner/example-repo.git",
				}},
			}
			_, err := LoadConfig(writeConfigForTest(t, raw))
			if err == nil || !strings.Contains(err.Error(), `unknown field "`+field+`"`) {
				t.Fatalf("旧字段 %s 应作为未知配置拒绝，err=%v", field, err)
			}
		})
	}
}
