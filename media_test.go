package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
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

var mediaTestRepo = &Repo{Name: "repo", Slug: "example-owner/ExampleSource", GHToken: "token"}

// mp4Bytes 是 Go 嗅探器认可的 mp4 魔数（box 锚定在偏移 4：ftyp 盒 + isom brand），
// 测试超大视频时只用头部 + 补零，不必真的构造 100MB 有效视频。
func mp4Bytes(size int) []byte {
	buf := bytes.Repeat([]byte{0}, size)
	copy(buf, []byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00isomiso2avc1mp41"))
	return buf
}

func newMediaTestServer(t *testing.T) *Server {
	t.Helper()
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/example-owner/ExampleSource" {
			_, _ = w.Write([]byte(`{"id":1026542182}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(github.Close)
	return &Server{
		cfg: &Config{
			MediaTempDir:              t.TempDir(),
			ImageDownloadHosts:        []string{"127.0.0.1"},
			ImageDownloadAllowPrivate: true,
			MediaSourceDir:            t.TempDir(),
			mediaTimeout:              10 * time.Second,
			mediaUploadLimit:          100,
		},
		gh:                 NewGitHub(github.URL, 10*time.Second),
		mediaLimiter:       newIssueRateLimiter(100),
		attachmentUploader: &fakeAttachmentUploader{},
	}
}

func mediaTempEntries(t *testing.T, s *Server) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(s.cfg.MediaTempDir)
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

	res := s.processMedia(context.Background(), mediaTestRepo, []string{img.URL})
	if res.imageCount != 1 {
		t.Fatalf("应附带 1 张截图，实际 %d，告警：%v", res.imageCount, res.warnings)
	}
	if len(res.warnings) != 0 {
		t.Errorf("不应有告警：%v", res.warnings)
	}
	uploader := s.attachmentUploader.(*fakeAttachmentUploader)
	if len(uploader.uploads) != 1 || !bytes.Equal(uploader.uploads[0].Bytes, pngBytes) {
		t.Fatalf("原生附件上传内容错误：%+v", uploader.uploads)
	}
	if !strings.Contains(res.md, "https://github.com/user-attachments/assets/") {
		t.Errorf("正文应引用 GitHub 原生附件：%q", res.md)
	}
	if entries := mediaTempEntries(t, s); len(entries) != 0 {
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
	res := s.processMedia(context.Background(), mediaTestRepo, []string{path})
	if res.imageCount != 1 || len(res.warnings) != 0 {
		t.Fatalf("本地路径应成功附带，imageCount=%d warnings=%v", res.imageCount, res.warnings)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("本地原文件不应被删：%v", err)
	}
	uploader := s.attachmentUploader.(*fakeAttachmentUploader)
	if len(uploader.uploads) != 1 || !bytes.Equal(uploader.uploads[0].Bytes, pngBytes) {
		t.Fatalf("本地文件未正确上传：%+v", uploader.uploads)
	}
}

func TestProcessMediaOversizedVideo(t *testing.T) {
	s := newMediaTestServer(t)
	dir := s.cfg.MediaSourceDir
	path := filepath.Join(dir, "big.mp4")
	if err := os.WriteFile(path, mp4Bytes(mediaMaxBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	res := s.processMedia(context.Background(), mediaTestRepo, []string{path})
	if res.imageCount+res.videoCount != 0 {
		t.Errorf("超大视频不应附带，imageCount=%d videoCount=%d", res.imageCount, res.videoCount)
	}
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "传给开发者") {
		t.Errorf("超大视频应提示传开发者，实际告警：%v", res.warnings)
	}
	if got := len(mediaTempEntries(t, s)); got != 0 {
		t.Errorf("超限媒体不应遗留临时文件，实际 %d 个", got)
	}
}

func TestProcessMediaUnsupportedType(t *testing.T) {
	s := newMediaTestServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("just some text"))
	}))
	defer srv.Close()
	res := s.processMedia(context.Background(), mediaTestRepo, []string{srv.URL})
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "类型不支持") {
		t.Errorf("应提示类型不支持，实际：%v", res.warnings)
	}
	if got := len(mediaTempEntries(t, s)); got != 0 {
		t.Errorf("非法媒体不应遗留临时文件，实际 %d 个", got)
	}
}

func TestProcessMediaDownload403(t *testing.T) {
	s := newMediaTestServer(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	res := s.processMedia(context.Background(), mediaTestRepo, []string{srv.URL})
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

func TestProcessMediaWithoutUploaderReportsFailure(t *testing.T) {
	s := &Server{cfg: &Config{}}
	res := s.processMedia(context.Background(), mediaTestRepo, []string{"whatever"})
	if res.requested != 1 || res.failed != 1 || res.md != "" ||
		len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "凭据") {
		t.Errorf("未配置附件上传器时应报告失败且不阻塞，实际 %+v", res)
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

	res := s.processMedia(context.Background(), mediaTestRepo, []string{img.URL})
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "白名单") {
		t.Errorf("应提示白名单拒绝，实际：%v", res.warnings)
	}
	if got := len(mediaTempEntries(t, s)); got != 0 {
		t.Errorf("被拒图片不应遗留临时文件，实际 %d 个", got)
	}
}

// TestProcessMediaLocalOutsideDir 覆盖本地路径收敛：目录外路径拒绝且不回显路径。
func TestProcessMediaLocalOutsideDir(t *testing.T) {
	s := newMediaTestServer(t)
	outside := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(outside, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	res := s.processMedia(context.Background(), mediaTestRepo, []string{outside})
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "不可用") {
		t.Errorf("目录外路径应拒绝，实际：%v", res.warnings)
	}
	if strings.Contains(res.warnings[0], outside) {
		t.Errorf("告警不应回显路径：%v", res.warnings)
	}
	if got := len(mediaTempEntries(t, s)); got != 0 {
		t.Errorf("目录外图片不应遗留临时文件，实际 %d 个", got)
	}
}

// TestProcessMediaUploadLimit 覆盖附件上传独立限额：额度耗尽后拒绝且不下载。
func TestProcessMediaUploadLimit(t *testing.T) {
	s := newMediaTestServer(t)
	s.mediaLimiter = newIssueRateLimiter(1)
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pngBytes)
	}))
	defer img.Close()

	first := s.processMedia(context.Background(), mediaTestRepo, []string{img.URL})
	if first.imageCount != 1 {
		t.Fatalf("首次应成功，imageCount=%d warnings=%v", first.imageCount, first.warnings)
	}
	second := s.processMedia(context.Background(), mediaTestRepo, []string{img.URL})
	if second.imageCount != 0 || len(second.warnings) != 1 || !strings.Contains(second.warnings[0], "上限") {
		t.Errorf("第二次应被限额拒绝，实际 imageCount=%d warnings=%v", second.imageCount, second.warnings)
	}
	if got := len(s.attachmentUploader.(*fakeAttachmentUploader).uploads); got != 1 {
		t.Errorf("应只上传 1 个媒体文件，实际 %d 个", got)
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

func TestCreateIssueSchemaAlwaysIncludesImages(t *testing.T) {
	s := &Server{cfg: &Config{}}
	schema := s.createIssueSchema("repo 说明")
	if _, ok := schema["properties"].(map[string]any)["images"]; !ok {
		t.Fatal("create_issue schema 应始终暴露 images 参数")
	}
	if _, err := json.Marshal(schema); err != nil {
		t.Fatalf("schema 应可序列化：%v", err)
	}
}

// TestProcessMediaPerItemUploadLimit 覆盖按项限额：额度只有 1 时，一次调用里的
// 两个合法图片只上传第一个，第二个以「上限」告警返回，而不是共享同一次配额。
func TestProcessMediaPerItemUploadLimit(t *testing.T) {
	s := newMediaTestServer(t)
	s.mediaLimiter = newIssueRateLimiter(1)
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(pngBytes)
	}))
	defer img.Close()

	res := s.processMedia(context.Background(), mediaTestRepo, []string{img.URL, img.URL})
	if res.imageCount != 1 {
		t.Errorf("额度为 1 时应只成功 1 张，实际 imageCount=%d", res.imageCount)
	}
	if len(res.warnings) != 1 || !strings.Contains(res.warnings[0], "上限") {
		t.Errorf("第二张应报上限告警，实际 warnings=%v", res.warnings)
	}
	if got := len(s.attachmentUploader.(*fakeAttachmentUploader).uploads); got != 1 {
		t.Errorf("应只上传 1 个媒体文件，实际 %d 个", got)
	}
}

// TestProcessMediaRelativePathUnderMediaSourceDir 覆盖相对路径解析：模型常给
// 相对文件名，images 里的相对路径应按 MediaSourceDir 解析，解析后位于目录内
// 即放行上传。
func TestProcessMediaRelativePathUnderMediaSourceDir(t *testing.T) {
	s := newMediaTestServer(t)
	if err := os.WriteFile(filepath.Join(s.cfg.MediaSourceDir, "capture.png"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	res := s.processMedia(context.Background(), mediaTestRepo, []string{"capture.png"})
	if res.imageCount != 1 || len(res.warnings) != 0 {
		t.Fatalf("相对路径 capture.png 应解析到 MediaSourceDir 并成功，imageCount=%d warnings=%v", res.imageCount, res.warnings)
	}
	if got := len(s.attachmentUploader.(*fakeAttachmentUploader).uploads); got != 1 {
		t.Errorf("应上传 1 个媒体文件，实际 %d 个", got)
	}
}

func loadMappedMediaConfig(t *testing.T, sourceDir string) *Config {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(map[string]any{
		"dataDir":           t.TempDir(),
		"mediaSourceDir":    sourceDir,
		"mediaSourcePrefix": "/AstrBot/data/temp",
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

	res := s.processMedia(context.Background(), mediaTestRepo, []string{"/AstrBot/data/temp/capture.png"})
	if res.imageCount != 1 || len(res.warnings) != 0 {
		t.Fatalf("容器路径应映射到媒体源目录并成功附带，imageCount=%d warnings=%v",
			res.imageCount, res.warnings)
	}
	if got := len(s.attachmentUploader.(*fakeAttachmentUploader).uploads); got != 1 {
		t.Errorf("应上传 1 个媒体文件，实际 %d 个", got)
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

	res := s.processMedia(context.Background(), mediaTestRepo, []string{"/AstrBot/data/temp/capture.png"})
	if res.imageCount != 1 || len(res.warnings) != 0 {
		t.Fatalf("相对媒体源目录不应被重复拼接，imageCount=%d warnings=%v",
			res.imageCount, res.warnings)
	}
	if got := len(s.attachmentUploader.(*fakeAttachmentUploader).uploads); got != 1 {
		t.Errorf("应上传 1 个媒体文件，实际 %d 个", got)
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
	res := s.processMedia(context.Background(), mediaTestRepo, []string{link})
	if res.imageCount != 0 || len(mediaTempEntries(t, s)) != 0 {
		t.Fatalf("指向目录外的符号链接应被拒绝且不遗留临时文件，imageCount=%d md=%q", res.imageCount, res.md)
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

type attachmentUploadSnapshot struct {
	Name        string
	ContentType string
	Size        int64
	Bytes       []byte
}

type fakeAttachmentUploader struct {
	failAt  map[int]error
	uploads []attachmentUploadSnapshot
}

func (f *fakeAttachmentUploader) Upload(_ context.Context, _ string, _ int64, in attachmentInput) (uploadedAttachment, error) {
	data, err := io.ReadAll(in.Reader)
	if err != nil {
		return uploadedAttachment{}, err
	}
	index := len(f.uploads)
	f.uploads = append(f.uploads, attachmentUploadSnapshot{
		Name:        in.Name,
		ContentType: in.ContentType,
		Size:        in.Size,
		Bytes:       data,
	})
	if err := f.failAt[index]; err != nil {
		return uploadedAttachment{}, err
	}
	ids := []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
	}
	return uploadedAttachment{
		Name: in.Name,
		URL:  "https://github.com/user-attachments/assets/" + ids[index],
	}, nil
}

func validPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var body bytes.Buffer
	if err := png.Encode(&body, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestProcessMediaNativeAttachmentsFailOpen(t *testing.T) {
	sourceDir := t.TempDir()
	tempDir := t.TempDir()
	pngBody := validPNG(t, 640, 480)
	var paths []string
	for i := range 3 {
		path := filepath.Join(sourceDir, fmt.Sprintf("capture-%d.png", i))
		if err := os.WriteFile(path, pngBody, 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	var repoIDCalls int
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/example-owner/ExampleSource" {
			repoIDCalls++
			_, _ = w.Write([]byte(`{"id":1026542182}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer github.Close()
	uploader := &fakeAttachmentUploader{failAt: map[int]error{
		1: fmt.Errorf("session expired: %w", errAttachmentSessionExpired),
	}}
	s := &Server{
		cfg: &Config{
			MediaSourceDir:   sourceDir,
			MediaTempDir:     tempDir,
			mediaTimeout:     10 * time.Second,
			mediaUploadLimit: 10,
		},
		gh:                 NewGitHub(github.URL, 10*time.Second),
		mediaLimiter:       newIssueRateLimiter(10),
		attachmentUploader: uploader,
		attachmentStatus:   newAttachmentStatusCache(attachmentStatus{Configured: true, Authenticated: true, Account: "github-attachment-bot"}),
	}
	repo := &Repo{Name: "example-source", Slug: "example-owner/ExampleSource", GHToken: "token"}
	res := s.processMedia(context.Background(), repo, paths)
	if res.requested != 3 || res.imageCount != 2 || res.failed != 1 {
		t.Fatalf("res=%+v", res)
	}
	if repoIDCalls != 1 {
		t.Fatalf("RepoID 调用=%d", repoIDCalls)
	}
	if len(uploader.uploads) != 3 || !bytes.Equal(uploader.uploads[0].Bytes, pngBody) {
		t.Fatalf("uploads=%+v", uploader.uploads)
	}
	for _, want := range []string{
		`<img width="640" height="480" alt="Image" src="https://github.com/user-attachments/assets/11111111-1111-4111-8111-111111111111" />`,
		`<img width="640" height="480" alt="Image" src="https://github.com/user-attachments/assets/33333333-3333-4333-8333-333333333333" />`,
	} {
		if !strings.Contains(res.md, want) {
			t.Fatalf("正文缺少 %q：%s", want, res.md)
		}
	}
	if strings.Contains(res.md, "session expired") || len(res.warnings) != 1 {
		t.Fatalf("md=%q warnings=%v", res.md, res.warnings)
	}
	if status := s.attachmentStatus.load(); status.Authenticated {
		t.Fatalf("Session 失效后健康状态仍为已认证：%+v", status)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("AstrBot 源文件被删除：%v", err)
		}
	}
}
