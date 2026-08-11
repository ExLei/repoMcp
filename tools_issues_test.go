package main

import (
	"strings"
	"testing"
)

// TestRenderIssueBody 覆盖正文渲染的段落结构契约：
// 通用段落（问题描述 / 复现 / 环境）+ 源码仓调研结论 + 署名（防双署名）。
func TestRenderIssueBody(t *testing.T) {
	src := &Repo{Name: "example-source", HasCode: true}
	fb := &Repo{Name: "pixkeep-feedback", HasCode: false}
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := srv.renderIssueBody(tt.r, tt.body, tt.evidence, tt.confirmed, tt.reporter, tt.repro, tt.env)
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
	fb := &Repo{Name: "pixkeep-feedback", HasCode: false}
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
