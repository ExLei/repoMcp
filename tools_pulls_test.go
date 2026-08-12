package main

import (
	"os"
	"strings"
	"testing"
)

func TestSplitReporter(t *testing.T) {
	cases := []struct {
		in       string
		wantNick string
		wantQQ   string
	}{
		{"张三(QQ12345)", "张三", "12345"},
		{"张三(qq12345)", "张三", "12345"},
		{"张三(12345)", "张三", "12345"},
		{"12345", "", "12345"},
		{"张三", "张三", ""},
		{"", "", ""},
		{" 李四 ( 99 ) ", "李四", "99"},
		{"(100000002)", "", "100000002"},
		{"　(100000002)", "", "100000002"}, // 昵称为全角空格：TrimSpace 后只剩 (100000002)
		{"()", "", ""},
	}
	for _, c := range cases {
		nick, qq := splitReporter(c.in)
		if nick != c.wantNick || qq != c.wantQQ {
			t.Errorf("splitReporter(%q) = (%q,%q), want (%q,%q)", c.in, nick, qq, c.wantNick, c.wantQQ)
		}
	}
}

func TestIsAdminReporter(t *testing.T) {
	srv := &Server{cfg: &Config{AdminReporters: []string{"管理员甲", "100000001", "老张(88001)"}}}
	admins := []string{"管理员甲", "管理员甲(QQ100000001)", "100000001", "老张", "老张(88001)", "路人(100000001)", "　(100000001)", "(100000001)"}
	others := []string{"", "路人", "路人(123)", "张三(QQ12345)"}
	for _, a := range admins {
		if !srv.isAdminReporter(a) {
			t.Errorf("isAdminReporter(%q) = false, want true", a)
		}
	}
	for _, a := range others {
		if srv.isAdminReporter(a) {
			t.Errorf("isAdminReporter(%q) = true, want false", a)
		}
	}
}

func newTestSrv() *Server {
	repos := []*Repo{
		{Name: "pixkeep", Slug: "example-owner/PixKeep-feedback", IssueRead: true},
		{Name: "qc", Slug: "example-owner/ExampleSource", IssueRead: true, IssueWrite: true},
	}
	return &Server{store: NewStore(repos), cfg: &Config{GitHubToken: "tok"}}
}

func TestResolveReadRepo(t *testing.T) {
	srv := newTestSrv()

	r, err := srv.resolveReadRepo(map[string]any{"repo": "pixkeep"})
	if err != nil || r.Slug != "example-owner/PixKeep-feedback" {
		t.Fatalf("短名解析失败: %v, %v", r, err)
	}

	r, err = srv.resolveReadRepo(map[string]any{"repo": "example-owner/AstrBot"})
	if err != nil || r.Slug != "example-owner/AstrBot" || r.GHToken != "tok" {
		t.Fatalf("任意公开仓库解析失败: %v, %v", r, err)
	}

	if _, err := srv.resolveReadRepo(map[string]any{"repo": "没有这个仓库"}); err == nil {
		t.Fatal("非法仓库名应报错")
	}

	if _, err := srv.resolveReadRepo(map[string]any{}); err == nil {
		t.Fatal("多配置仓库时省略 repo 应报错")
	}

	// 未接入 issue 的短名应拒绝
	noIssue := &Server{store: NewStore([]*Repo{{Name: "plain", Slug: "a/b"}}), cfg: &Config{}}
	if _, err := noIssue.resolveReadRepo(map[string]any{"repo": "plain"}); err == nil {
		t.Fatal("未接入 issue 的短名应报错")
	}
	// 只有一个可读仓库时允许省略
	single := &Server{store: NewStore([]*Repo{{Name: "plain", Slug: "a/b", IssueRead: true}}), cfg: &Config{}}
	r, err = single.resolveReadRepo(map[string]any{})
	if err != nil {
		t.Fatalf("单仓库省略 repo 应成功: %v", err)
	}
}

func TestResolveAdminWriteRepo(t *testing.T) {
	srv := newTestSrv()

	// 管理员：任意公开仓库
	r, err := srv.resolveAdminWriteRepo(map[string]any{"repo": "other/Repo"}, nil)
	if err != nil || r.Slug != "other/Repo" || r.GHToken != "tok" {
		t.Fatalf("管理员任意仓库失败: %v, %v", r, err)
	}
	// 管理员：配置短名
	r, err = srv.resolveAdminWriteRepo(map[string]any{"repo": "qc"}, nil)
	if err != nil || r.Name != "qc" {
		t.Fatalf("管理员配置短名失败: %v, %v", r, err)
	}
	// 非法
	if _, err := srv.resolveAdminWriteRepo(map[string]any{"repo": "xx"}, nil); err == nil {
		t.Fatal("非法仓库应报错")
	}
}

func TestExternalAdmins(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/cmd_config.json"
	bom := "\xef\xbb\xbf"
	write := func(s string) {
		if err := os.WriteFile(path, []byte(bom+s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"admins_id": ["astrbot", "100000002"]}`)

	srv := &Server{cfg: &Config{AstrbotAdminsFile: path}}
	if !srv.isAdminReporter("100000002") {
		t.Fatal("admins_id 中的 QQ 号应视为管理员")
	}
	if !srv.isAdminReporter("管理员甲(QQ100000002)") {
		t.Fatal("QQ 前缀的 reporter 应命中 admins_id")
	}
	if srv.isAdminReporter("路人(123)") {
		t.Fatal("非管理员不应命中")
	}
	write(`{"admins_id": ["astrbot", "888"]}`)
	if !srv.isAdminReporter("888") {
		t.Fatal("mtime 变化后应重读新名单")
	}
	if srv.isAdminReporter("100000002") {
		t.Fatal("旧名单不应再命中")
	}
	plain := &Server{cfg: &Config{}}
	if plain.isAdminReporter("888") {
		t.Fatal("未配置 astrbotAdminsFile 时不应命中")
	}
}

// TestPullStateText 覆盖 PR 状态渲染优先级：已合并 > 草稿 > 已关闭 > 开放中。
// 草稿 PR（state=open + draft=true）必须与「开放中」区分，否则模型会把
// 未就绪的 PR 当成可审核的开放 PR。
func TestPullStateText(t *testing.T) {
	cases := []struct {
		p    PR
		want string
	}{
		{PR{State: "open"}, "开放中"},
		{PR{State: "open", Draft: true}, "草稿"},
		{PR{State: "closed"}, "已关闭"},
		{PR{State: "closed", Merged: true}, "已合并"},
		{PR{State: "open", Draft: true, Merged: true}, "已合并"}, // 合并优先于草稿（理论上不可能同时出现）
	}
	for _, c := range cases {
		if got := pullStateText(c.p); got != c.want {
			t.Errorf("pullStateText(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
}

func TestPullMetaLine(t *testing.T) {
	line := pullMetaLine(PR{Author: "example-owner", UpdatedAt: "2026-08-01"})
	if !strings.Contains(line, "example-owner") || !strings.Contains(line, "2026-08-01") {
		t.Errorf("pullMetaLine 输出缺字段: %q", line)
	}
}
