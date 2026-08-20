package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pngBytes 是足以通过 http.DetectContentType 嗅探的最小 PNG 魔数。
var pngBytes = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}

// mp4Bytes 是 Go 嗅探器认可的 mp4 魔数（box 锚定在偏移 4：ftyp 盒 + isom brand），
// 测试超大视频时只用头部 + 补零，不必真的构造 100MB 有效视频。
func mp4Bytes(size int) []byte {
	buf := bytes.Repeat([]byte{0}, size)
	copy(buf, []byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00isomiso2avc1mp41"))
	return buf
}

func newMediaTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		cfg: &Config{
			MediaStoreDir:             t.TempDir(),
			MediaPublicBaseURL:        "https://astrbot.example/issue-media",
			mediaPublicPath:           "/issue-media",
			MediaTempDir:              t.TempDir(),
			ImageDownloadHosts:        []string{"127.0.0.1"},
			ImageDownloadAllowPrivate: true,
			MediaSourceDir:            t.TempDir(),
			mediaUploadLimit:          100,
		},
		mediaLimiter: newIssueRateLimiter(100),
	}
}

func storedMediaEntries(t *testing.T, s *Server) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(s.cfg.MediaStoreDir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestProcessMediaImageURL(t *testing.T) {
	s := newMediaTestServer(t)
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := r.Header.Get("Cookie"); c != "ck=1" {
			t.Errorf("Cookie 应透传，实际 %q", c)
		}
		_, _ = w.Write(pngBytes)
	}))
	defer img.Close()
	s.cfg.ImageDownloadCookie = "ck=1"

	res := s.processMedia(context.Background(), "repo", []string{img.URL})
	if res.imageCount != 1 {
		t.Fatalf("应附带 1 张截图，实际 %d，告警：%v", res.imageCount, res.warnings)
	}
	if len(res.warnings) != 0 {
		t.Errorf("不应有告警：%v", res.warnings)
	}
	entries := storedMediaEntries(t, s)
	if len(entries) != 1 {
		t.Fatalf("应保存 1 个媒体文件，实际 %d", len(entries))
	}
	if want := "![截图](" + s.cfg.MediaPublicBaseURL + "/" + entries[0].Name() + ")"; res.md != want {
		t.Errorf("渲染链接：got %q want %q", res.md, want)
	}
	stored, err := os.ReadFile(filepath.Join(s.cfg.MediaStoreDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, pngBytes) {
		t.Error("持久文件内容应等于下载的图片")
	}
	if entries := storedMediaEntries(t, &Server{cfg: &Config{MediaStoreDir: s.cfg.MediaTempDir}}); len(entries) != 0 {
		t.Errorf("下载临时文件应已清理，剩余 %d 个", len(entries))
	}
}

func TestProcessMediaLocalPath(t *testing.T) {
	s := newMediaTestServer(t)
	dir := s.cfg.MediaSourceDir
	path := filepath.Join(dir, "local.png")
	if err := os.WriteFile(path, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	res := s.processMedia(context.Background(), "repo", []string{path})
	if res.imageCount != 1 || len(res.warnings) != 0 {
		t.Fatalf("本地路径应成功附带，imageCount=%d warnings=%v", res.imageCount, res.warnings)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("本地原文件不应被删：%v", err)
	}
	if got := len(storedMediaEntries(t, s)); got != 1 {
		t.Errorf("应保存 1 个媒体文件，实际 %d", got)
	}
}

func loadServerMediaConfig(t *testing.T, sourceDir, storeDir string) *Config {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(map[string]any{
		"dataDir":            t.TempDir(),
		"mediaSourceDir":     sourceDir,
		"mediaStoreDir":      storeDir,
		"mediaPublicBaseURL": "https://astrbot.example/issue-media",
		"repos": []map[string]any{{
			"name": "repo",
			"url":  "https://github.com/example-owner/test.git",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("本地媒体存储配置应有效：%v", err)
	}
	return cfg
}

func TestLoadConfigRejectsUnsafeMediaPublicPath(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(map[string]any{
		"mediaStoreDir":      t.TempDir(),
		"mediaPublicBaseURL": "https://astrbot.example/issue-media/%7B",
		"repos": []map[string]any{{
			"name": "repo",
			"url":  "https://github.com/example-owner/test.git",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(configPath); err == nil {
		t.Fatal("包含 ServeMux 模式字符的 mediaPublicBaseURL 应被拒绝")
	}
}

func TestProcessMediaStoresOnServer(t *testing.T) {
	sourceDir := t.TempDir()
	storeDir := t.TempDir()
	cfg := loadServerMediaConfig(t, sourceDir, storeDir)
	s := &Server{
		cfg:          cfg,
		mediaLimiter: newIssueRateLimiter(100),
	}
	sourcePath := filepath.Join(sourceDir, "capture.png")
	if err := os.WriteFile(sourcePath, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	res := s.processMedia(context.Background(), "repo", []string{sourcePath})
	if res.imageCount != 1 || len(res.warnings) != 0 {
		t.Fatalf("图片应保存到服务器，imageCount=%d warnings=%v", res.imageCount, res.warnings)
	}
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("媒体目录应只有 1 个文件，实际 %d", len(entries))
	}
	storedPath := filepath.Join(storeDir, entries[0].Name())
	stored, err := os.ReadFile(storedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, pngBytes) {
		t.Error("服务器文件内容应等于原始图片")
	}
	if want := "![截图](https://astrbot.example/issue-media/" + entries[0].Name() + ")"; res.md != want {
		t.Errorf("正文应引用服务器 URL：got %q want %q", res.md, want)
	}
	if info, err := os.Stat(storedPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o644 {
		t.Errorf("持久媒体权限应为 0644，实际 %04o", info.Mode().Perm())
	}
}

func TestHandlePublicMediaContract(t *testing.T) {
	storeDir := t.TempDir()
	name := "20260822-010203-abcdef123456.png"
	if err := os.WriteFile(filepath.Join(storeDir, name), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	linkName := "20260822-010204-abcdef123457.png"
	outside := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(outside, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(storeDir, linkName)); err != nil {
		t.Skipf("环境不支持符号链接：%v", err)
	}
	s := &Server{cfg: loadServerMediaConfig(t, t.TempDir(), storeDir)}
	tests := []struct {
		name      string
		method    string
		path      string
		status    int
		wantBody  bool
		wantEmpty bool
		wantAllow string
	}{
		{name: "get", method: http.MethodGet, path: "/issue-media/" + name, status: http.StatusOK, wantBody: true},
		{name: "head", method: http.MethodHead, path: "/issue-media/" + name, status: http.StatusOK, wantEmpty: true},
		{name: "listing", method: http.MethodGet, path: "/issue-media/", status: http.StatusNotFound},
		{name: "traversal", method: http.MethodGet, path: "/issue-media/../secret.png", status: http.StatusNotFound},
		{name: "symlink", method: http.MethodGet, path: "/issue-media/" + linkName, status: http.StatusNotFound},
		{name: "post", method: http.MethodPost, path: "/issue-media/" + name, status: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.handlePublicMedia(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.status {
				t.Fatalf("status=%d want=%d body=%q", rec.Code, tt.status, rec.Body.String())
			}
			if tt.wantBody && !bytes.Equal(rec.Body.Bytes(), pngBytes) {
				t.Error("GET 应返回原始图片")
			}
			if tt.wantEmpty && rec.Body.Len() != 0 {
				t.Errorf("HEAD 响应体应为空，实际 %d 字节", rec.Body.Len())
			}
			if got := rec.Header().Get("Allow"); got != tt.wantAllow {
				t.Errorf("Allow=%q want=%q", got, tt.wantAllow)
			}
			if tt.status == http.StatusOK {
				if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
					t.Errorf("Cache-Control=%q", got)
				}
			}
		})
	}
}

func TestServerHandlerBareMediaPrefix(t *testing.T) {
	s := &Server{cfg: loadServerMediaConfig(t, t.TempDir(), t.TempDir())}
	handler := s.handler()
	tests := []struct {
		method    string
		status    int
		wantAllow string
	}{
		{method: http.MethodGet, status: http.StatusNotFound},
		{method: http.MethodPost, status: http.StatusMethodNotAllowed, wantAllow: "GET, HEAD"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(tt.method, "/issue-media", nil))
		if rec.Code != tt.status {
			t.Errorf("%s status=%d want=%d", tt.method, rec.Code, tt.status)
		}
		if got := rec.Header().Get("Allow"); got != tt.wantAllow {
			t.Errorf("%s Allow=%q want=%q", tt.method, got, tt.wantAllow)
		}
	}
}

func TestProcessMediaOversizedVideo(t *testing.T) {
	s := newMediaTestServer(t)
	dir := s.cfg.MediaSourceDir
	path := filepath.Join(dir, "big.mp4")
	if err := os.WriteFile(path, mp4Bytes(mediaMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	res := s.processMedia(context.Background(), "repo", []string{path})
	if res.imageCount+res.videoCount != 0 {
		t.Errorf("超大视频不应附带，imageCount=%d videoCount=%d", res.imageCount, res.videoCount)
	}
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "传给开发者") {
		t.Errorf("超大视频应提示传开发者，实际告警：%v", res.warnings)
	}
	if got := len(storedMediaEntries(t, s)); got != 0 {
		t.Errorf("超限媒体不应落盘，实际 %d 个", got)
	}
}

func TestProcessMediaUnsupportedType(t *testing.T) {
	s := newMediaTestServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("just some text"))
	}))
	defer srv.Close()
	res := s.processMedia(context.Background(), "repo", []string{srv.URL})
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "类型不支持") {
		t.Errorf("应提示类型不支持，实际：%v", res.warnings)
	}
	if got := len(storedMediaEntries(t, s)); got != 0 {
		t.Errorf("非法媒体不应落盘，实际 %d 个", got)
	}
}

func TestProcessMediaDownload403(t *testing.T) {
	s := newMediaTestServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	res := s.processMedia(context.Background(), "repo", []string{srv.URL})
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "Cookie") {
		t.Errorf("403 应提示 Cookie 配置，实际：%v", res.warnings)
	}
}

// 署名前带 --- 分隔线：附件段插在分隔线之前，保持 分隔线+署名 整体。
func TestInsertMediaSectionSeparator(t *testing.T) {
	withSep := "## 问题描述\n\n正文。\n\n---\n由聊天机器人代 张三（群聊反馈，经 人机 转提交）\n"
	got := insertMediaSection(withSep, "![截图](https://x/c.png)")
	if idx := strings.Index(got, "## 附件"); idx == -1 || idx > strings.Index(got, "---") {
		t.Errorf("附件段应插在 --- 分隔线之前：%s", got)
	}
	if !strings.Contains(got, "---\n由聊天机器人代") {
		t.Errorf("分隔线与署名应保持相邻：%s", got)
	}
}

func TestProcessMediaNoStore(t *testing.T) {
	s := &Server{cfg: &Config{}}
	res := s.processMedia(context.Background(), "repo", []string{"whatever"})
	if res.md != "" || res.imageCount != 0 || res.videoCount != 0 {
		t.Errorf("未配置服务器媒体存储时应完全跳过，实际 %+v", res)
	}
}

// TestProcessMediaHostNotWhitelisted 覆盖 SSRF/Cookie 防线：host 不在白名单时
// 拒绝下载、不带 Cookie、不落盘。
func TestProcessMediaHostNotWhitelisted(t *testing.T) {
	s := newMediaTestServer(t)
	s.cfg.ImageDownloadHosts = []string{"example.com"} // 127.0.0.1 不在白名单
	s.cfg.ImageDownloadCookie = "ck=secret"
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c := r.Header.Get("Cookie"); c != "" {
			t.Errorf("非白名单域名不应收到 Cookie，实际 %q", c)
		}
		_, _ = w.Write(pngBytes)
	}))
	defer img.Close()

	res := s.processMedia(context.Background(), "repo", []string{img.URL})
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "白名单") {
		t.Errorf("应提示白名单拒绝，实际：%v", res.warnings)
	}
	if got := len(storedMediaEntries(t, s)); got != 0 {
		t.Errorf("被拒图片不应落盘，实际 %d 个", got)
	}
}

// TestProcessMediaLocalOutsideDir 覆盖本地路径收敛：目录外路径拒绝且不回显路径。
func TestProcessMediaLocalOutsideDir(t *testing.T) {
	s := newMediaTestServer(t)
	outside := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(outside, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	res := s.processMedia(context.Background(), "repo", []string{outside})
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "不可用") {
		t.Errorf("目录外路径应拒绝，实际：%v", res.warnings)
	}
	if strings.Contains(res.warnings[0], outside) {
		t.Errorf("告警不应回显路径：%v", res.warnings)
	}
	if got := len(storedMediaEntries(t, s)); got != 0 {
		t.Errorf("目录外图片不应落盘，实际 %d 个", got)
	}
}

// TestProcessMediaUploadLimit 覆盖媒体保存独立限额：额度耗尽后拒绝且不下载。
func TestProcessMediaUploadLimit(t *testing.T) {
	s := newMediaTestServer(t)
	s.mediaLimiter = newIssueRateLimiter(1)
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pngBytes)
	}))
	defer img.Close()

	first := s.processMedia(context.Background(), "repo", []string{img.URL})
	if first.imageCount != 1 {
		t.Fatalf("首次应成功，imageCount=%d warnings=%v", first.imageCount, first.warnings)
	}
	second := s.processMedia(context.Background(), "repo", []string{img.URL})
	if second.imageCount != 0 || len(second.warnings) != 1 || !strings.Contains(second.warnings[0], "上限") {
		t.Errorf("第二次应被限额拒绝，实际 imageCount=%d warnings=%v", second.imageCount, second.warnings)
	}
	if got := len(storedMediaEntries(t, s)); got != 1 {
		t.Errorf("应只保存 1 个媒体文件，实际 %d 个", got)
	}
}

func TestInsertMediaSection(t *testing.T) {
	withSig := "## 问题描述\n\n正文内容。\n\n---\n由聊天机器人代 张三（群聊反馈，经 人机 转提交）\n"
	got := insertMediaSection(withSig, "![截图](https://x/a.png)")
	if !strings.Contains(got, "## 附件\n\n![截图](https://x/a.png)") {
		t.Errorf("应插入附件段：%s", got)
	}
	if idx := strings.Index(got, "## 附件"); idx > strings.Index(got, "由聊天机器人代") {
		t.Errorf("附件段应在署名之前：%s", got)
	}
	noSig := "## 问题描述\n\n正文。"
	got = insertMediaSection(noSig, "![截图](https://x/b.png)")
	if !strings.HasSuffix(got, "## 附件\n\n![截图](https://x/b.png)\n") {
		t.Errorf("无署名时应追加文末：%s", got)
	}
}

// TestSweepOrphanMedia 覆盖服务器孤儿媒体清扫：旧孤儿删除、被引用保留、
// 新文件保留、非本服务命名不动。
func TestSweepOrphanMedia(t *testing.T) {
	storeDir := t.TempDir()
	oldStamp := time.Now().UTC().Add(-8 * 24 * time.Hour).Format("20060102-150405")
	freshStamp := time.Now().UTC().Format("20060102-150405")
	orphan := oldStamp + "-deadbeef0001.png"
	referenced := oldStamp + "-deadbeef0002.png"
	fresh := freshStamp + "-deadbeef0003.png"
	for _, name := range []string{orphan, referenced, fresh, "README.md"} {
		if err := os.WriteFile(filepath.Join(storeDir, name), pngBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/search/issues") {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.URL.Query().Get("q"), "deadbeef0002") {
			_, _ = w.Write([]byte(`{"total_count":1,"items":[{"number":1,"title":"x","state":"open"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
	}))
	t.Cleanup(srv.Close)
	s := &Server{
		cfg:   &Config{MediaStoreDir: storeDir, MediaPublicBaseURL: "https://astrbot.example/issue-media", mediaTimeout: 10 * time.Second},
		gh:    NewGitHub(srv.URL, 10*time.Second),
		store: NewStore([]*Repo{{Name: "fb", Slug: "example-owner/ExampleFeedback", IssueRead: true, GHToken: "t"}}),
	}

	s.sweepOrphanMedia(context.Background())
	if _, err := os.Stat(filepath.Join(storeDir, orphan)); !os.IsNotExist(err) {
		t.Error("超过宽限期且无人引用的媒体应删除")
	}
	for _, name := range []string{referenced, fresh, "README.md"} {
		if _, err := os.Stat(filepath.Join(storeDir, name)); err != nil {
			t.Errorf("%s 应保留：%v", name, err)
		}
	}
}

func TestSweepOrphanMediaSearchError(t *testing.T) {
	storeDir := t.TempDir()
	oldStamp := time.Now().UTC().Add(-8 * 24 * time.Hour).Format("20060102-150405")
	name := oldStamp + "-deadbeef0001.png"
	if err := os.WriteFile(filepath.Join(storeDir, name), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	s := &Server{
		cfg:   &Config{MediaStoreDir: storeDir, MediaPublicBaseURL: "https://astrbot.example/issue-media", mediaTimeout: 10 * time.Second},
		gh:    NewGitHub(srv.URL, 10*time.Second),
		store: NewStore([]*Repo{{Name: "fb", Slug: "example-owner/ExampleFeedback", IssueRead: true, GHToken: "t"}}),
	}

	s.sweepOrphanMedia(context.Background())
	if _, err := os.Stat(filepath.Join(storeDir, name)); err != nil {
		t.Fatalf("引用核验失败时不得删除媒体：%v", err)
	}
}

func TestSweepMediaDir(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "media-old.bin")
	fresh := filepath.Join(dir, "media-fresh.bin")
	if err := os.WriteFile(old, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-mediaMaxAge - time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if n := sweepMediaDir(dir); n != 1 {
		t.Errorf("应清理 1 个孤儿文件，实际 %d", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("过期文件应被删除")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("新文件应保留")
	}
}

func TestCreateIssueSchemaImagesConditional(t *testing.T) {
	s := &Server{cfg: &Config{}}
	schema := s.createIssueSchema("repo 说明")
	if _, ok := schema["properties"].(map[string]any)["images"]; ok {
		t.Error("未配置服务器媒体存储时不应暴露 images 参数")
	}
	s.cfg.MediaStoreDir = t.TempDir()
	s.cfg.MediaPublicBaseURL = "https://astrbot.example/issue-media"
	schema = s.createIssueSchema("repo 说明")
	if _, ok := schema["properties"].(map[string]any)["images"]; !ok {
		t.Fatal("配置服务器媒体存储后应暴露 images 参数")
	}
	if _, err := json.Marshal(schema); err != nil {
		t.Fatalf("schema 应可序列化：%v", err)
	}
}

// TestProcessMediaPerItemUploadLimit 覆盖按项限额：额度只有 1 时，一次调用里的
// 两个合法图片只保存第一个，第二个以「上限」告警返回，而不是共享同一次配额。
func TestProcessMediaPerItemUploadLimit(t *testing.T) {
	s := newMediaTestServer(t)
	s.mediaLimiter = newIssueRateLimiter(1)
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pngBytes)
	}))
	defer img.Close()

	res := s.processMedia(context.Background(), "repo", []string{img.URL, img.URL})
	if res.imageCount != 1 {
		t.Errorf("额度为 1 时应只成功 1 张，实际 imageCount=%d", res.imageCount)
	}
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "上限") {
		t.Errorf("第二张应报上限告警，实际 warnings=%v", res.warnings)
	}
	if got := len(storedMediaEntries(t, s)); got != 1 {
		t.Errorf("应只保存 1 个媒体文件，实际 %d 个", got)
	}
}

// TestProcessMediaRelativePathUnderMediaSourceDir 覆盖相对路径解析：模型常给
// 相对文件名，images 里的相对路径应按 MediaSourceDir 解析，解析后位于目录内
// 即放行保存。
func TestProcessMediaRelativePathUnderMediaSourceDir(t *testing.T) {
	s := newMediaTestServer(t)
	if err := os.WriteFile(filepath.Join(s.cfg.MediaSourceDir, "capture.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	res := s.processMedia(context.Background(), "repo", []string{"capture.png"})
	if res.imageCount != 1 || len(res.warnings) != 0 {
		t.Fatalf("相对路径 capture.png 应解析到 MediaSourceDir 并成功，imageCount=%d warnings=%v", res.imageCount, res.warnings)
	}
	if got := len(storedMediaEntries(t, s)); got != 1 {
		t.Errorf("应保存 1 个媒体文件，实际 %d 个", got)
	}
}

func loadMappedMediaConfig(t *testing.T, sourceDir string) *Config {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(map[string]any{
		"dataDir":            t.TempDir(),
		"mediaStoreDir":      t.TempDir(),
		"mediaPublicBaseURL": "https://astrbot.example/issue-media",
		"mediaSourceDir":     sourceDir,
		"mediaSourcePrefix":  "/AstrBot/data/temp",
		"repos": []map[string]any{{
			"name": "repo",
			"url":  "https://github.com/example-owner/test.git",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("带 mediaSourcePrefix 的配置应有效：%v", err)
	}
	return cfg
}

// TestProcessMediaMappedSourcePrefix 覆盖 AstrBot 容器路径到 repoMcp 宿主机路径的映射：
// 模型看到 /AstrBot/data/temp，服务端实际读取 /opt/astrbot/data/temp。
func TestProcessMediaMappedSourcePrefix(t *testing.T) {
	s := newMediaTestServer(t)
	sourceDir := t.TempDir()
	s.cfg = loadMappedMediaConfig(t, sourceDir)
	if err := os.WriteFile(filepath.Join(sourceDir, "capture.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	res := s.processMedia(context.Background(), "repo",
		[]string{"/AstrBot/data/temp/capture.png"})
	if res.imageCount != 1 || len(res.warnings) != 0 {
		t.Fatalf("容器路径应映射到媒体源目录并成功附带，imageCount=%d warnings=%v",
			res.imageCount, res.warnings)
	}
	if got := len(storedMediaEntries(t, s)); got != 1 {
		t.Errorf("应保存 1 个媒体文件，实际 %d 个", got)
	}
}

func TestProcessMediaMappedSourcePrefixWithRelativeRoot(t *testing.T) {
	s := newMediaTestServer(t)
	relativeSourceDir, err := os.MkdirTemp(".", "media-source-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(relativeSourceDir) })
	s.cfg = loadMappedMediaConfig(t, relativeSourceDir)
	if err := os.WriteFile(filepath.Join(relativeSourceDir, "capture.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	res := s.processMedia(context.Background(), "repo",
		[]string{"/AstrBot/data/temp/capture.png"})
	if res.imageCount != 1 || len(res.warnings) != 0 {
		t.Fatalf("相对媒体源目录不应被重复拼接，imageCount=%d warnings=%v",
			res.imageCount, res.warnings)
	}
	if got := len(storedMediaEntries(t, s)); got != 1 {
		t.Errorf("应保存 1 个媒体文件，实际 %d 个", got)
	}
}

// TestProcessMediaSymlinkEscapeRejected 覆盖符号链接逃逸：MediaSourceDir 内指向
// 目录外文件的软链必须被拒绝且不落盘，不能把目录外内容读进 issue。
func TestProcessMediaSymlinkEscapeRejected(t *testing.T) {
	s := newMediaTestServer(t)
	outside := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(outside, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.cfg.MediaSourceDir, "link.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("环境不支持符号链接：%v", err)
	}
	res := s.processMedia(context.Background(), "repo", []string{link})
	if res.imageCount != 0 || len(storedMediaEntries(t, s)) != 0 {
		t.Fatalf("指向目录外的符号链接应被拒绝且不落盘，imageCount=%d md=%q", res.imageCount, res.md)
	}
	if len(res.warnings) == 0 {
		t.Error("拒绝时应给出告警")
	}
}

// TestSweepMediaDirKeepsUnrelatedFiles 覆盖临时目录清扫的命名边界：只删本服务
// 命名模式（media-*.bin）的过期文件；同目录里同样过期的无关文件与新的服务
// 临时文件都要保留。
func TestSweepMediaDirKeepsUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	own := filepath.Join(dir, "media-old.bin")
	unrelated := filepath.Join(dir, "notes.txt")
	fresh := filepath.Join(dir, "media-fresh.bin")
	for _, f := range []string{own, unrelated, fresh} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-mediaMaxAge - time.Hour)
	if err := os.Chtimes(own, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unrelated, past, past); err != nil {
		t.Fatal(err)
	}
	if n := sweepMediaDir(dir); n != 1 {
		t.Errorf("应只清理 1 个本服务过期文件，实际 %d", n)
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Error("过期的服务临时文件应被删除")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Error("过期的无关文件应保留")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("新的服务临时文件应保留")
	}
}

// TestSweepOrphanMediaGlobalSearch 覆盖引用核验的搜索范围：hex token 命中的
// issue 可能在任意仓库（含未配置仓库），引用核验必须用全局查询；只查配置仓库
// 会漏掉未配置仓库里的引用，把仍被引用的媒体误删。
func TestSweepOrphanMediaGlobalSearch(t *testing.T) {
	storeDir := t.TempDir()
	oldStamp := time.Now().UTC().Add(-8 * 24 * time.Hour).Format("20060102-150405")
	name := oldStamp + "-deadbeef0001.png"
	if err := os.WriteFile(filepath.Join(storeDir, name), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/search/issues") {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, "deadbeef0001") || strings.Contains(q, "repo:") {
			t.Errorf("全局检索应只含 token、不限定 repo，实际 q=%q", q)
		}
		_, _ = w.Write([]byte(`{"total_count":1,"items":[{"number":9,"title":"引用在未配置仓库","state":"open"}]}`))
	}))
	t.Cleanup(srv.Close)
	s := &Server{
		cfg: &Config{
			MediaStoreDir:      storeDir,
			MediaPublicBaseURL: "https://astrbot.example/issue-media",
			GitHubToken:        "gtok",
			mediaTimeout:       10 * time.Second,
		},
		gh:    NewGitHub(srv.URL, 10*time.Second),
		store: NewStore([]*Repo{{Name: "fb", Slug: "example-owner/ExampleFeedback", IssueRead: true, GHToken: "t"}}),
	}

	s.sweepOrphanMedia(context.Background())
	if _, err := os.Stat(filepath.Join(storeDir, name)); err != nil {
		t.Fatalf("未配置仓库的 issue 已引用时不得删除媒体：%v", err)
	}
}
