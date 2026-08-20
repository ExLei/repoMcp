package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRenderIssueBody 覆盖正文渲染的段落结构契约：
// 通用段落（问题描述 / 复现 / 环境）+ 源码仓调研结论 + 附件段 + 署名（防双署名）。
func TestRenderIssueBody(t *testing.T) {
	src := &Repo{Name: "example-source", HasCode: true}
	fb := &Repo{Name: "example-feedback", HasCode: false}
	srv := &Server{store: NewStore(nil)} // 无仓库：shortHead 返回空，署名回退 "-"

	tests := []struct {
		name      string
		r         *Repo
		body      string
		evidence  string
		confirmed bool
		reporter  string
		repro     string
		env       string
		media     string
		wantHas   []string
		wantNot   []string
		wantOrder []string // 段落相对顺序（结构固定契约）
	}{
		{
			name: "源码仓 confirmed：全段落 + 索引 commit 署名",
			r:    src,
			body: "截图后工具栏不显示。", evidence: "src/engine.rs:42 工具栏绘制处",
			confirmed: true, reporter: "张三(QQ12345)",
			repro: "1. 截图\n2. 点标注", env: "v0.1.0 / Windows 11 / 安装包 / 截图",
			wantHas: []string{"## 问题描述", "## 复现 / 触发条件", "## 环境", "## 调研结论", "已定位：",
				"由聊天机器人代 张三(QQ12345)（源码反馈", "索引 commit -"},
			wantOrder: []string{"## 问题描述", "## 复现 / 触发条件", "## 环境", "## 调研结论"},
		},
		{
			name: "源码仓 unconfirmed 且无 repro：无复现段落",
			r:    src,
			body: "偶发闪退。", evidence: "未定位", confirmed: false,
			reporter: "李四", repro: "", env: "v0.2.0 / Windows 10 / 便携版 / 其他",
			wantHas:   []string{"## 问题描述", "## 环境", "## 调研结论", "未定位：需维护者核实"},
			wantNot:   []string{"## 复现 / 触发条件"},
			wantOrder: []string{"## 问题描述", "## 环境", "## 调研结论"},
		},
		{
			name: "反馈仓库：无调研结论段，署名标注群聊反馈",
			r:    fb,
			body: "工具栏消失。", evidence: "", confirmed: false,
			reporter: "王五(QQ9)", repro: "打开软件后", env: "v0.1.0 / Windows 11 / 安装包 / 截图",
			wantHas: []string{"## 问题描述", "## 复现 / 触发条件", "## 环境", "群聊反馈"},
			wantNot: []string{"## 调研结论", "已定位", "未定位", "源码反馈"},
		},
		{
			name: "body 已含署名：不追加，防双署名",
			r:    src,
			body: "问题描述。由聊天机器人代 张三 提交", evidence: "x", confirmed: true,
			reporter: "张三", repro: "", env: "v0.1.0 / Windows 11 / 安装包 / 截图",
			wantHas: []string{"## 环境"},
			wantNot: []string{"（源码反馈"},
		},
		{
			name: "带附件：附件段位于署名之前",
			r:    fb,
			body: "工具栏消失。", evidence: "", confirmed: false,
			reporter: "王五(QQ9)", repro: "", env: "v0.1.0 / Windows 11 / 安装包 / 截图",
			media:     "![截图](https://astrbot.example/issue-media/a.png)",
			wantHas:   []string{"## 附件", "![截图](https://", "群聊反馈"},
			wantNot:   []string{"## 调研结论"},
			wantOrder: []string{"## 环境", "## 附件"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := srv.renderIssueBody(tt.r, tt.body, tt.evidence, tt.confirmed, tt.reporter, tt.repro, tt.env, tt.media)
			for _, s := range tt.wantHas {
				if !strings.Contains(got, s) {
					t.Errorf("缺少 %q\n--- 实际输出 ---\n%s", s, got)
				}
			}
			for _, s := range tt.wantNot {
				if strings.Contains(got, s) {
					t.Errorf("不应包含 %q\n--- 实际输出 ---\n%s", s, got)
				}
			}
			prev, prevSection := -1, ""
			for _, s := range tt.wantOrder {
				idx := strings.Index(got, s)
				if idx == -1 {
					t.Fatalf("顺序断言目标 %q 不存在\n--- 实际输出 ---\n%s", s, got)
				}
				if idx <= prev {
					t.Errorf("段落顺序错误：%q 出现在 %q 之前\n--- 实际输出 ---\n%s", s, prevSection, got)
				}
				prev, prevSection = idx, s
			}
		})
	}
}

// TestValidateCreateArgs 覆盖 create_issue 的必填与调研字段校验：
// env 必填、confidence 按仓库类型分支、evidence 校验、reporter/title/body 长度。
func TestValidateCreateArgs(t *testing.T) {
	src := &Repo{Name: "example-source", HasCode: true}
	fb := &Repo{Name: "example-feedback", HasCode: false}
	longBody := "测试正文内容需要超过二十个字才能通过长度校验要求哦"

	tests := []struct {
		name       string
		r          *Repo
		reporter   string
		title      string
		body       string
		env        string
		confidence string
		evidence   string
		wantOK     bool
		wantErrHas string
	}{
		{name: "反馈仓全合法且 confidence 省略", r: fb, reporter: "张三", title: "[Bug] 测试", body: longBody,
			env: "v0.1.0 / Windows 11 / 安装包 / 截图", confidence: "", evidence: "", wantOK: true},
		{name: "反馈仓 evidence 不校验", r: fb, reporter: "张三", title: "[Bug] 测试", body: longBody,
			env: "v0.1.0 / Windows 11 / 安装包 / 截图", confidence: "unconfirmed", evidence: "", wantOK: true},
		{name: "反馈仓拒绝 confirmed", r: fb, reporter: "张三", title: "[Bug] 测试", body: longBody,
			env: "v0.1.0 / Windows 11 / 安装包 / 截图", confidence: "confirmed", evidence: "a.go:1",
			wantOK: false, wantErrHas: "请改用 confidence=unconfirmed"},
		{name: "源码仓 confidence 省略被拒", r: src, reporter: "张三", title: "[Bug] 测试", body: longBody,
			env: "v0.1.0 / Windows 11 / 安装包 / 截图", confidence: "", evidence: "",
			wantOK: false, wantErrHas: "源码仓库必填 confidence"},
		{name: "源码仓 unconfirmed 但 evidence 空被拒", r: src, reporter: "张三", title: "[Bug] 测试", body: longBody,
			env: "v0.1.0 / Windows 11 / 安装包 / 截图", confidence: "unconfirmed", evidence: "",
			wantOK: false, wantErrHas: "evidence 不能省略"},
		{name: "源码仓 confirmed 但 evidence 无出处被拒", r: src, reporter: "张三", title: "[Bug] 测试", body: longBody,
			env: "v0.1.0 / Windows 11 / 安装包 / 截图", confidence: "confirmed", evidence: "没找到",
			wantOK: false, wantErrHas: "路径:行号"},
		{name: "源码仓 confirmed 全合法", r: src, reporter: "张三(QQ12345)", title: "[Bug] 测试", body: longBody,
			env: "v0.1.0 / Windows 11 / 安装包 / 截图", confidence: "confirmed", evidence: "engine.go:42 工具栏绘制处",
			wantOK: true},
		{name: "env 空被拒", r: src, reporter: "张三", title: "[Bug] 测试", body: longBody,
			env: "", confidence: "unconfirmed", evidence: "未定位", wantOK: false, wantErrHas: "env 必填"},
		{name: "reporter 空被拒", r: src, reporter: "", title: "[Bug] 测试", body: longBody,
			env: "v0.1.0", confidence: "unconfirmed", evidence: "未定位", wantOK: false, wantErrHas: "reporter 必填"},
		{name: "title 过短被拒", r: src, reporter: "张三", title: "[Bug]", body: longBody,
			env: "v0.1.0", confidence: "unconfirmed", evidence: "未定位", wantOK: false, wantErrHas: "title 长度"},
		{name: "body 过短被拒", r: src, reporter: "张三", title: "[Bug] 测试", body: "太短",
			env: "v0.1.0", confidence: "unconfirmed", evidence: "未定位", wantOK: false, wantErrHas: "body 太短"},
		{name: "confidence 非法值被拒", r: src, reporter: "张三", title: "[Bug] 测试", body: longBody,
			env: "v0.1.0", confidence: "maybe", evidence: "未定位", wantOK: false, wantErrHas: "必须是 confirmed 或 unconfirmed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			confirmed, err := validateCreateArgs(tt.r, tt.reporter, tt.title, tt.body, tt.env, tt.confidence, tt.evidence)
			if tt.wantOK {
				if err != nil {
					t.Fatalf("应通过校验，实际报错：%v", err)
				}
				if tt.confidence == "confirmed" && !confirmed {
					t.Error("confirmed 应解析为 true")
				}
				return
			}
			if err == nil {
				t.Fatal("应被拒绝，实际通过")
			}
			if !strings.Contains(err.Error(), tt.wantErrHas) {
				t.Errorf("错误文案应包含 %q，实际：%v", tt.wantErrHas, err)
			}
		})
	}
}

// TestStripIssuePrefix 覆盖五种前缀剥离与无前缀透传（D2：查重打分前剥离）。
func TestStripIssuePrefix(t *testing.T) {
	cases := map[string]string{
		"[Bug] 崩溃":       "崩溃",
		"[Feature] 批量导出": "批量导出",
		"[Question] 怎么用": "怎么用",
		"[许愿] 导出":        "导出",
		"[争议] 工具栏":       "工具栏",
		"  [Bug] x  ":    "x",
		"普通标题":           "普通标题",
	}

	for in, want := range cases {
		if got := stripIssuePrefix(in); got != want {
			t.Errorf("stripIssuePrefix(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestValidateCreateArgsTitlePrefix 覆盖标题前缀硬校验（spec §9 五种前缀）。
func TestValidateCreateArgsTitlePrefix(t *testing.T) {
	fb := &Repo{Name: "example-feedback", HasCode: false}
	longBody := "测试正文内容需要超过二十个字才能通过长度校验要求哦"
	valid := []string{"[Bug] 崩溃", "[Feature] 批量导出", "[Question] 怎么用", "[许愿] 导出", "[争议] 工具栏"}
	for _, title := range valid {
		if _, err := validateCreateArgs(fb, "张三", title, longBody, "v0.1.0", "", ""); err != nil {
			t.Errorf("前缀 %q 应通过，实际：%v", title, err)
		}
	}
	for _, title := range []string{"软件崩溃无法启动了", "希望支持批量导出图片", " 截图后工具栏消失"} {
		if _, err := validateCreateArgs(fb, "张三", title, longBody, "v0.1.0", "", ""); err == nil {
			t.Errorf("无前缀标题 %q 应被拒绝", title)
		} else if !strings.Contains(err.Error(), "必须以") {
			t.Errorf("错误文案应提示前缀要求，实际：%v", err)
		}
	}
}

// TestStripPrefixDedupSimilarity 覆盖 D2 的动机场景：
// 不带剥离时「[许愿] 导出」与「批量导出」重叠系数低于阈值漏判，剥离后应命中。
func TestStripPrefixDedupSimilarity(t *testing.T) {
	newTitle := "[许愿] 导出"
	existing := "批量导出"
	raw := ghSimilarity(ghTextTokens(newTitle), ghTextTokens(existing))
	stripped := ghSimilarity(ghTextTokens(stripIssuePrefix(newTitle)), ghTextTokens(stripIssuePrefix(existing)))
	if raw >= issueDupThreshold {
		t.Fatalf("前提不成立：未剥离时 %v 应低于阈值 %v", raw, issueDupThreshold)
	}
	if stripped < issueDupThreshold {
		t.Errorf("剥离后相似度 %v 应达到阈值 %v", stripped, issueDupThreshold)
	}
}

// TestIssueStateText 覆盖 issue 状态渲染：开放/已关闭/已解决/不予处理，
// 全部中文输出，避免模型把英文 open/closed 照抄进回复。
func TestIssueStateText(t *testing.T) {
	cases := []struct {
		it   Issue
		want string
	}{
		{Issue{State: "open"}, "开放中"},
		{Issue{State: "open", Reason: "reopened"}, "开放中"},
		{Issue{State: "closed"}, "已关闭"},
		{Issue{State: "closed", Reason: "completed"}, "closed/已解决"},
		{Issue{State: "closed", Reason: "not_planned"}, "closed/不予处理"},
		{Issue{State: "closed", Reason: "reopened"}, "已关闭"},
	}
	for _, c := range cases {
		if got := issueStateText(c.it); got != c.want {
			t.Errorf("issueStateText(%+v) = %q，期望 %q", c.it, got, c.want)
		}
	}
}

// TestCreateIssueSchemaHasRepro 覆盖 create_issue 参数 schema 与处理器的契约：
// toolCreateIssue 读取 repro 参数渲染「复现」段，生成的 JSON schema 就必须声明该
// 属性，否则客户端按 schema 校验后根本无法传入复现步骤。断言产物 schema 而非源码文本。
func TestCreateIssueSchemaHasRepro(t *testing.T) {
	s := &Server{
		store: NewStore([]*Repo{{Name: "qc", Slug: "example-owner/demo", IssueRead: true, IssueWrite: true}}),
		cfg:   &Config{GitHubToken: "tok"},
	}
	for _, def := range s.issueToolDefs() {
		if def.Name != "create_issue" {
			continue
		}
		props, ok := def.Schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("create_issue schema 缺少 properties：%#v", def.Schema)
		}
		p, ok := props["repro"]
		if !ok {
			t.Fatalf("create_issue schema 缺少 repro 属性（toolCreateIssue 仍读取 repro 渲染复现段）")
		}
		if p.(map[string]any)["type"] != "string" {
			t.Errorf("create_issue schema repro 应为 string 类型，实际 %#v", p)
		}
		return
	}
	t.Fatal("未找到 create_issue 工具定义（需要可写仓库）")
}

// TestToolListReleasesRendersTagDateName 覆盖 list_releases 输出契约：
// 每条 release 必须渲染 tag、发布日期与（与 tag 不同的）发布名称，模型据此回答
// 「最新版本是什么」。走真实工具处理器 + 假 GitHub HTTP（media_test 同一模式），
// 断言处理器输出的实际文本。
func TestToolListReleasesRendersTagDateName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/example-owner/demo/releases" {
			_, _ = w.Write([]byte(`[{"tag_name":"v1.2.0","name":"春日大更新","published_at":"2026-08-20T10:00:00Z","body":"修复了进度条闪烁"}]`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	s := &Server{
		store: NewStore(nil),
		cfg:   &Config{MaxResponseBytes: 8192},
		gh:    NewGitHub(srv.URL, 10*time.Second),
	}
	out, err := s.toolListReleases(context.Background(), map[string]any{"repo": "example-owner/demo"})
	if err != nil {
		t.Fatalf("toolListReleases：%v", err)
	}
	for _, want := range []string{"v1.2.0", "2026-08-20", "春日大更新"} {
		if !strings.Contains(out, want) {
			t.Errorf("list_releases 输出应包含 %q（tag / 发布日期 / 发布名称），实际输出：\n%s", want, out)
		}
	}
}
