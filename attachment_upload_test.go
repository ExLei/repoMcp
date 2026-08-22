package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAttachmentSessionRequiresPrivateRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session")
	if err := os.WriteFile(path, []byte("secret-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAttachmentSession(path); err == nil {
		t.Fatal("0644 Session 文件必须被拒绝")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadAttachmentSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret-value" {
		t.Fatalf("Session=%q", got)
	}
}

func TestLoadAttachmentSessionRejectsUnsafeKinds(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		path func(t *testing.T) string
	}{
		{
			name: "directory",
			path: func(t *testing.T) string { return dir },
		},
		{
			name: "empty",
			path: func(t *testing.T) string {
				path := filepath.Join(dir, "empty")
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "symlink",
			path: func(t *testing.T) string {
				target := filepath.Join(dir, "target")
				if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(dir, "link")
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("当前平台不支持软链：%v", err)
				}
				return link
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadAttachmentSession(tt.path(t)); err == nil {
				t.Fatal("不安全的 Session 路径必须被拒绝")
			}
		})
	}
}

func newIdentityUploaderForTest(t *testing.T, handler http.Handler, expected string) (*nativeAttachmentUploader, *url.URL) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	uploader, err := buildNativeAttachmentUploader("session-secret", expected, base, srv.Client(), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return uploader, base
}

func TestNativeAttachmentIdentityUsesSessionCookiePair(t *testing.T) {
	var sawCookies bool
	uploader, base := newIdentityUploaderForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/settings/profile" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		first, err := r.Cookie("user_session")
		if err != nil || first.Value != "session-secret" {
			t.Fatalf("user_session=%v err=%v", first, err)
		}
		second, err := r.Cookie("__Host-user_session_same_site")
		if err != nil || second.Value != "session-secret" {
			t.Fatalf("same-site cookie=%v err=%v", second, err)
		}
		sawCookies = true
		http.SetCookie(w, &http.Cookie{Name: "_gh_sess", Value: "rotated", Path: "/", Secure: true, HttpOnly: true})
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><meta name="user-login" content="github-attachment-bot"></head></html>`))
	}), "github-attachment-bot")

	got, err := uploader.checkIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "github-attachment-bot" || !sawCookies {
		t.Fatalf("account=%q sawCookies=%v", got, sawCookies)
	}
	var rotated bool
	for _, cookie := range uploader.githubClient.Jar.Cookies(base) {
		if cookie.Name == "_gh_sess" && cookie.Value == "rotated" {
			rotated = true
		}
	}
	if !rotated {
		t.Fatal("GitHub 轮换的 _gh_sess 未保存在 Cookie Jar")
	}
}

func TestNativeAttachmentIdentityRejectsExpiredOrWrongAccount(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		handler  http.Handler
		want     error
	}{
		{
			name:     "expired",
			expected: "github-attachment-bot",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "/login")
				w.WriteHeader(http.StatusFound)
			}),
			want: errAttachmentSessionExpired,
		},
		{
			name:     "wrong-account",
			expected: "github-attachment-bot",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`<meta name="user-login" content="example-owner">`))
			}),
			want: errAttachmentAccountMismatch,
		},
		{
			name:     "missing-meta",
			expected: "github-attachment-bot",
			handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`<html></html>`))
			}),
			want: errAttachmentAccountUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uploader, _ := newIdentityUploaderForTest(t, tt.handler, tt.expected)
			_, err := uploader.checkIdentity(context.Background())
			if err == nil || !errors.Is(err, tt.want) {
				t.Fatalf("err=%v", err)
			}
			if strings.Contains(err.Error(), "session-secret") {
				t.Fatal(errors.New("错误泄露 Session"))
			}
		})
	}
}

func TestNativeAttachmentUploadDetectsExpiredSessionWhenTokenMissing(t *testing.T) {
	uploader, _ := newIdentityUploaderForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example-owner/ExampleSource":
			_, _ = w.Write([]byte(`<html>public repository</html>`))
		case "/settings/profile":
			w.Header().Set("Location", "/login")
			w.WriteHeader(http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}), "github-attachment-bot")

	_, err := uploader.Upload(context.Background(), "example-owner/ExampleSource", 1026542182, attachmentInput{
		Name:        "capture.png",
		ContentType: "image/png",
		Size:        3,
		Reader:      bytes.NewReader([]byte("png")),
	})
	if err == nil || !errors.Is(err, errAttachmentSessionExpired) {
		t.Fatalf("缺少 uploadToken 且身份页跳转登录时应判定 Session 失效，err=%v", err)
	}
}

func TestHealthReportsAttachmentUploaderStatusWithoutAffectingReadiness(t *testing.T) {
	tests := []struct {
		name   string
		status attachmentStatus
	}{
		{
			name: "not-configured",
		},
		{
			name: "configured-not-authenticated",
			status: attachmentStatus{
				Configured: true,
				Account:    "github-attachment-bot",
			},
		},
		{
			name: "authenticated",
			status: attachmentStatus{
				Configured:    true,
				Authenticated: true,
				Account:       "github-attachment-bot",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				cfg: &Config{
					GitHubAttachmentSessionFile: "/opt/repomcp/secrets/session-secret",
					GitHubAttachmentAccount:     "github-attachment-bot",
				},
				store:            NewStore(nil),
				index:            NewIndex(),
				attachmentStatus: newAttachmentStatusCache(tt.status),
			}
			request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			response := httptest.NewRecorder()
			s.handleHealth(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("附件认证状态不得影响 ready，HTTP=%d body=%s", response.Code, response.Body.String())
			}
			var payload struct {
				Ready              bool             `json:"ready"`
				AttachmentUploader attachmentStatus `json:"attachmentUploader"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if !payload.Ready {
				t.Fatal("无源码仓待索引时 ready 应保持 true")
			}
			if payload.AttachmentUploader != tt.status {
				t.Fatalf("attachmentUploader=%+v want %+v", payload.AttachmentUploader, tt.status)
			}
			for _, forbidden := range []string{
				"session-secret",
				"/opt/repomcp/secrets",
				"user_session",
				"__Host-user_session_same_site",
				"uploadToken",
			} {
				if strings.Contains(response.Body.String(), forbidden) {
					t.Fatalf("健康响应泄露 %q：%s", forbidden, response.Body.String())
				}
			}
		})
	}
}

type capturedMultipartPart struct {
	name     string
	filename string
	value    []byte
}

func captureMultipart(r *http.Request) ([]capturedMultipartPart, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, err
	}
	var parts []capturedMultipartPart
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return parts, nil
		}
		if err != nil {
			return nil, err
		}
		value, err := io.ReadAll(part)
		if err != nil {
			_ = part.Close()
			return nil, err
		}
		parts = append(parts, capturedMultipartPart{
			name:     part.FormName(),
			filename: part.FileName(),
			value:    value,
		})
		_ = part.Close()
	}
}

func multipartNames(parts []capturedMultipartPart) []string {
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		names = append(names, part.name)
	}
	return names
}

func TestNativeAttachmentUploadProtocol(t *testing.T) {
	fileBytes := []byte("native-attachment-bytes")
	finalURL := "https://github.com/user-attachments/assets/11111111-1111-4111-8111-111111111111"

	var githubURL string
	var s3Parts []capturedMultipartPart
	s3 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			http.Error(w, "credentials leaked", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Origin") != githubURL {
			http.Error(w, "wrong origin", http.StatusBadRequest)
			return
		}
		parts, err := captureMultipart(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s3Parts = parts
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s3.Close()

	github := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/example-owner/ExampleFeedback":
			if _, err := r.Cookie("user_session"); err != nil {
				http.Error(w, "missing session", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`<script>{"uploadToken":"repo-upload-token"}</script>`))
		case r.Method == http.MethodPost && r.URL.Path == "/upload/policies/assets":
			if r.Header.Get("Accept") != "application/json" ||
				r.Header.Get("Origin") != githubURL ||
				r.Header.Get("Referer") != githubURL+"/example-owner/ExampleFeedback" ||
				r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
				http.Error(w, "wrong policy headers", http.StatusBadRequest)
				return
			}
			parts, err := captureMultipart(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			wantNames := []string{"name", "size", "content_type", "authenticity_token", "repository_id"}
			if fmt.Sprint(multipartNames(parts)) != fmt.Sprint(wantNames) {
				http.Error(w, "wrong policy field order", http.StatusBadRequest)
				return
			}
			wantValues := []string{"capture.png", fmt.Sprint(len(fileBytes)), "image/png", "repo-upload-token", "1325294260"}
			for i, want := range wantValues {
				if string(parts[i].value) != want {
					http.Error(w, "wrong policy value", http.StatusBadRequest)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"upload_url": s3.URL,
				"asset": map[string]any{
					"id":           99,
					"name":         "capture.png",
					"size":         len(fileBytes),
					"content_type": "image/png",
					"href":         finalURL,
				},
				"form": map[string]string{
					"key":                          "asset-key",
					"acl":                          "private",
					"policy":                       "s3-policy",
					"X-Amz-Algorithm":              "AWS4-HMAC-SHA256",
					"X-Amz-Credential":             "credential",
					"X-Amz-Date":                   "20260822T000000Z",
					"X-Amz-Signature":              "signature",
					"Content-Type":                 "image/png",
					"Cache-Control":                "max-age=31557600",
					"x-amz-meta-Surrogate-Control": "max-age=31557600",
					"zzz-extra":                    "last",
					"aaa-extra":                    "first",
				},
				"asset_upload_url":                "/upload/assets/99",
				"asset_upload_authenticity_token": "finalize-token",
			})
		case r.Method == http.MethodPut && r.URL.Path == "/upload/assets/99":
			if r.Header.Get("Origin") != githubURL ||
				r.Header.Get("Referer") != githubURL+"/example-owner/ExampleFeedback" ||
				r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
				http.Error(w, "wrong finalize headers", http.StatusBadRequest)
				return
			}
			parts, err := captureMultipart(r)
			if err != nil || len(parts) != 1 || parts[0].name != "authenticity_token" || string(parts[0].value) != "finalize-token" {
				http.Error(w, "wrong finalize token", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"href": finalURL, "name": "capture.png"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer github.Close()
	githubURL = github.URL

	base, err := url.Parse(github.URL)
	if err != nil {
		t.Fatal(err)
	}
	uploader, err := buildNativeAttachmentUploader("session-secret", "github-attachment-bot", base, github.Client(), s3.Client())
	if err != nil {
		t.Fatal(err)
	}
	uploader.validateUploadDestination = func(got *url.URL) error {
		if got.String() != s3.URL {
			return fmt.Errorf("unexpected upload URL %s", got.Redacted())
		}
		return nil
	}
	uploaded, err := uploader.Upload(context.Background(), "example-owner/ExampleFeedback", 1325294260, attachmentInput{
		Name:        "capture.png",
		ContentType: "image/png",
		Size:        int64(len(fileBytes)),
		Reader:      bytes.NewReader(fileBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.URL != finalURL || uploaded.Name != "capture.png" {
		t.Fatalf("uploaded=%+v", uploaded)
	}
	wantS3Names := []string{
		"key", "acl", "policy", "X-Amz-Algorithm", "X-Amz-Credential",
		"X-Amz-Date", "X-Amz-Signature", "Content-Type", "Cache-Control",
		"x-amz-meta-Surrogate-Control", "aaa-extra", "zzz-extra", "file",
	}
	if fmt.Sprint(multipartNames(s3Parts)) != fmt.Sprint(wantS3Names) {
		t.Fatalf("S3 fields=%v", multipartNames(s3Parts))
	}
	last := s3Parts[len(s3Parts)-1]
	if last.filename != "capture.png" || !bytes.Equal(last.value, fileBytes) {
		t.Fatalf("file name=%q bytes=%q", last.filename, last.value)
	}
}

type readCountingReader struct {
	reader io.Reader
	reads  int
}

func (r *readCountingReader) Read(p []byte) (int, error) {
	r.reads++
	return r.reader.Read(p)
}

func TestStreamingMultipartDoesNotPrebufferFile(t *testing.T) {
	fileBytes := bytes.Repeat([]byte("x"), 8<<20)
	counting := &readCountingReader{reader: bytes.NewReader(fileBytes)}
	body, contentType, contentLength, err := streamingMultipart(
		[]multipartField{{Name: "key", Value: "asset-key"}},
		"file",
		"capture.png",
		counting,
		int64(len(fileBytes)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if counting.reads != 0 {
		t.Fatalf("构造 multipart 时提前读取文件 %d 次", counting.reads)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data; boundary=") {
		t.Fatalf("Content-Type=%q", contentType)
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) != contentLength || !bytes.Contains(raw, fileBytes) {
		t.Fatalf("body=%d Content-Length=%d", len(raw), contentLength)
	}
}

func TestValidateNativeAttachmentURL(t *testing.T) {
	valid := "https://github.com/user-attachments/assets/11111111-1111-4111-8111-111111111111"
	if _, err := validateNativeAttachmentURL(valid); err != nil {
		t.Fatal(err)
	}
	invalid := []string{
		"http://github.com/user-attachments/assets/11111111-1111-4111-8111-111111111111",
		"https://evil.example/user-attachments/assets/11111111-1111-4111-8111-111111111111",
		"https://github.com/user-attachments/assets/not-a-uuid",
		"https://github.com/user-attachments/assets/11111111-1111-4111-8111-111111111111?secret=x",
		"https://user@github.com/user-attachments/assets/11111111-1111-4111-8111-111111111111",
	}
	for _, raw := range invalid {
		t.Run(raw, func(t *testing.T) {
			if _, err := validateNativeAttachmentURL(raw); err == nil {
				t.Fatal("非法原生附件 URL 必须被拒绝")
			}
		})
	}
}

func TestResolveFinalizeURLRequiresMatchingAssetID(t *testing.T) {
	base, err := url.Parse("https://github.com")
	if err != nil {
		t.Fatal(err)
	}
	uploader := &nativeAttachmentUploader{githubBase: base}
	got, err := uploader.resolveFinalizeURL("/upload/assets/99", 99)
	if err != nil || got.String() != "https://github.com/upload/assets/99" {
		t.Fatalf("got=%v err=%v", got, err)
	}
	invalid := []struct {
		raw     string
		assetID int64
	}{
		{raw: "/upload/assets/98", assetID: 99},
		{raw: "/upload/assets/099", assetID: 99},
		{raw: "https://github.com/upload/assets/99", assetID: 99},
		{raw: "/upload/assets/99?token=secret", assetID: 99},
		{raw: "/upload/assets/99", assetID: 0},
	}
	for _, tt := range invalid {
		t.Run(fmt.Sprintf("%s-%d", tt.raw, tt.assetID), func(t *testing.T) {
			if _, err := uploader.resolveFinalizeURL(tt.raw, tt.assetID); err == nil {
				t.Fatal("finalize URL 与 policy asset.id 不匹配时必须拒绝")
			}
		})
	}
}
