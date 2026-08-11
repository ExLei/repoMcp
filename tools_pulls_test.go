package main

import (
	"strings"
	"testing"
)

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
