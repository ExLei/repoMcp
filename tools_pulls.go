// tools_pulls.go：PR 查询工具（只读）。
//
// 与 issue 检索一样支持任意公开仓库（repo 用 owner/name），
// 只做查询，不做任何写入——PR 的写操作不在本服务能力内。
package main

import (
	"context"
	"fmt"
	"strings"
)

// pullStateText 把 PR 状态渲染为中文：已合并优先于已关闭；草稿是未就绪的开放 PR，
// 必须与「开放中」区分开，否则模型会把草稿当成可审核的开放 PR。
func pullStateText(p PR) string {
	if p.Merged {
		return "已合并"
	}
	if p.Draft {
		return "草稿"
	}
	if p.State == "closed" {
		return "已关闭"
	}
	return "开放中"
}

// pullToolDefs 产出 PR 查询工具。只读，不依赖配置仓库，任何部署都挂载。
func (s *Server) pullToolDefs() []toolDef {
	repoDesc := "仓库：配置短名，或任意公开仓库 owner/name（如 example-owner/AstrBot）；只有一个配置仓库时可省略"
	return []toolDef{
		{
			Name:  "search_pulls",
			Title: "检索 PR",
			Desc: "列出仓库的 Pull Request（默认只列未合并的）。回答「有哪些 PR / 这个功能有人提 PR 吗 / PR 什么状态」。" +
				"只读查询，支持任意公开仓库。每条 PR 状态直接标注：[草稿] 未就绪不可合并 / [开放中] 待审核 / [已关闭] 未合并关闭 / [已合并]。" +
				"按标注如实回答状态，不要自行从 state 字段推断。",
			Schema: obj(map[string]any{
				"repo":  str(repoDesc),
				"state": str("open（默认，未合并）/ closed（已关闭或已合并）/ all"),
				"query": str("标题关键词过滤，可选"),
				"limit": integer("返回条数，默认 10", 1, 30),
			}),
			Handle: s.toolSearchPulls,
		},
		{
			Name:  "read_pull",
			Title: "读取 PR",
			Desc: "按编号读取单个 PR 的完整描述、状态（草稿/开放/已关闭/已合并）、作者与分支信息。" +
				"在 search_pulls 拿到候选后，需要看「这个 PR 具体改了什么」时用它。",
			Schema: obj(map[string]any{
				"repo":   str(repoDesc),
				"number": integer("PR 编号（不带 #）", 1, 1000000),
			}, "number"),
			Handle: s.toolReadPull,
		},
		{
			Name:  "list_pull_comments",
			Title: "PR 评论",
			Desc:  "列出 PR 的讨论评论（作者/日期/内容）。只读，支持任意公开仓库。",
			Schema: obj(map[string]any{
				"repo":   str(repoDesc),
				"number": integer("PR 编号（不带 #）", 1, 1000000),
				"limit":  integer("返回条数，默认 20", 1, 30),
			}, "number"),
			Handle: s.toolListPullComments,
		},
	}
}

// ── search_pulls ───────────────────────────────────────────

func (s *Server) toolSearchPulls(ctx context.Context, args map[string]any) (string, error) {
	r, err := s.resolveReadRepo(args)
	if err != nil {
		return "", err
	}
	state := argStr(args, "state")
	query := strings.ToLower(strings.TrimSpace(argStr(args, "query")))
	limit := argInt(args, "limit", 10)

	prs, err := s.gh.ListPRs(ctx, r, state, limit)
	if err != nil {
		return "", err
	}
	if query != "" {
		filtered := prs[:0]
		for _, p := range prs {
			if strings.Contains(strings.ToLower(p.Title), query) {
				filtered = append(filtered, p)
			}
		}
		prs = filtered
	}

	w := newBudget(s.cfg.MaxResponseBytes)
	scope := fmt.Sprintf("%s 的 PR（状态 %s）", r.Slug, ghNormState(state))
	if query != "" {
		scope = fmt.Sprintf("%s 中标题含 %q 的 PR（状态 %s）", r.Slug, query, ghNormState(state))
	}
	if len(prs) == 0 {
		w.line("未找到匹配的 PR：" + scope + "。")
		w.line("提示：查看已关闭/已合并的 PR 用 state=all 或 state=closed。")
		return w.String(), nil
	}
	w.line(fmt.Sprintf("%s，命中 %d 条：", scope, len(prs)))
	for _, p := range prs {
		w.line("")
		if !w.line(fmt.Sprintf("#%d [%s] %s", p.Number, pullStateText(p), truncate(p.Title, 160))) {
			break
		}
		w.line("    " + pullMetaLine(p))
		if p.URL != "" {
			w.line("    " + p.URL)
		}
	}
	w.line("")
	w.line("要看 PR 详情与描述，用 read_pull + 编号；看讨论用 list_pull_comments。")
	return w.String(), nil
}

// ── read_pull ──────────────────────────────────────────────

func (s *Server) toolReadPull(ctx context.Context, args map[string]any) (string, error) {
	r, err := s.resolveReadRepo(args)
	if err != nil {
		return "", err
	}
	number := argInt(args, "number", 0)
	if number <= 0 {
		return "", fmt.Errorf("number 必须是正整数（PR 编号）")
	}
	p, err := s.gh.GetPR(ctx, r, number)
	if err != nil {
		return "", err
	}

	w := newBudget(s.cfg.MaxResponseBytes)
	w.line(fmt.Sprintf("#%d [%s] %s", p.Number, pullStateText(p), p.Title))
	w.line(pullMetaLine(p))
	if p.URL != "" {
		w.line(p.URL)
	}
	if p.HeadRef != "" && p.BaseRef != "" {
		w.line(fmt.Sprintf("分支：%s → %s", p.HeadRef, p.BaseRef))
	}
	w.line("")
	body := strings.TrimSpace(p.Body)
	if body == "" {
		w.line("（描述为空）")
	} else {
		for _, l := range strings.Split(body, "\n") {
			if !w.line(truncate(l, 300)) {
				break
			}
		}
	}
	w.line("")
	w.line("看讨论评论用 list_pull_comments + 编号。")
	return w.String(), nil
}

// ── list_pull_comments ─────────────────────────────────────

func (s *Server) toolListPullComments(ctx context.Context, args map[string]any) (string, error) {
	r, err := s.resolveReadRepo(args)
	if err != nil {
		return "", err
	}
	number := argInt(args, "number", 0)
	if number <= 0 {
		return "", fmt.Errorf("number 必须是正整数（PR 编号）")
	}
	comments, err := s.gh.PRComments(ctx, r, number, argInt(args, "limit", 20))
	if err != nil {
		return "", err
	}

	w := newBudget(s.cfg.MaxResponseBytes)
	if len(comments) == 0 {
		w.line(fmt.Sprintf("%s 的 PR #%d 暂无讨论评论。", r.Slug, number))
		return w.String(), nil
	}
	w.line(fmt.Sprintf("%s 的 PR #%d 讨论（最近 %d 条）：", r.Slug, number, len(comments)))
	for _, c := range comments {
		w.line("")
		if !w.line(fmt.Sprintf("%s  %s：", c.Date, c.Author)) {
			break
		}
		stop := false
		for _, l := range strings.Split(strings.TrimSpace(c.Body), "\n") {
			if strings.TrimSpace(l) == "" {
				continue
			}
			if !w.line("  " + truncate(l, 240)) {
				stop = true
				break
			}
		}
		if stop {
			break
		}
	}
	return w.String(), nil
}

// pullMetaLine 渲染 PR 的单行元信息（作者 / 更新时间）。
func pullMetaLine(p PR) string {
	author := strings.TrimSpace(p.Author)
	if author == "" {
		author = "-"
	}
	updated := strings.TrimSpace(p.UpdatedAt)
	if updated == "" {
		updated = "-"
	}
	return fmt.Sprintf("作者 %s · 更新 %s", author, updated)
}
