// tools_issues.go：issue 的检索、创建与管理工具，以及它们的服务端护栏。
//
// 为什么护栏必须落在服务端：消费方是 IM 里的小模型。「先调研再提 issue」「别重复提」
// 「别随手关」写进工具描述只是建议，模型照不照做不可控，而这些工具是**会真实写入
// 别人仓库**的。因此凡是能硬校验的都在这里拦：
//   - 查重由服务端强制执行，模型跳不过；
//   - 创建有每小时频率上限，防止对话里刷 issue；
//   - 标签必须是仓库已有的，不让机器人污染标签体系；
//   - 状态变更必须给结论说明，一次只能动一个 issue，且不能重复关闭；
//   - 正文由服务端按模板渲染，强制带上「确定性 + 证据 + 提交来源」。
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// issueDupThreshold 是判定「疑似重复」的标题相似度阈值（重叠系数）。
	// 偏低是刻意的：误报只让模型多确认一次，漏报则直接产生重复 issue。
	issueDupThreshold = 0.55
	issueDupMax       = 5

	issueTitleMinRunes = 6
	issueTitleMaxRunes = 200
	issueBodyMinRunes  = 20
	issueEvidMinRunes  = 10
	issueNoteMinRunes  = 10
)

// ── 频率限制 ────────────────────────────────────────────────

// issueRateLimiter 限制单仓每小时的 issue 创建量。
// 配额在调用 GitHub **之前**扣除：创建失败也照扣，宁可少提也不能让失败重试变成刷屏。
type issueRateLimiter struct {
	mu      sync.Mutex
	perHour int // 0 = 不限
	hist    map[string][]time.Time
}

func newIssueRateLimiter(perHour int) *issueRateLimiter {
	return &issueRateLimiter{perHour: perHour, hist: make(map[string][]time.Time)}
}

// take 扣一次配额。失败时返回距下一个可用名额的等待时长。
func (l *issueRateLimiter) take(repo string) (bool, time.Duration) {
	if l.perHour <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	kept := l.hist[repo][:0]
	for _, t := range l.hist[repo] {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	l.hist[repo] = kept
	if len(kept) >= l.perHour {
		return false, time.Hour - now.Sub(kept[0])
	}
	l.hist[repo] = append(kept, now)
	return true, 0
}

// remaining 返回当前小时内剩余可创建数；不限额时返回 -1。
func (l *issueRateLimiter) remaining(repo string) int {
	if l.perHour <= 0 {
		return -1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	n := 0
	for _, t := range l.hist[repo] {
		if now.Sub(t) < time.Hour {
			n++
		}
	}
	return max(l.perHour-n, 0)
}

// ── 仓库解析 ────────────────────────────────────────────────

// issueRepos 返回具备指定 issue 能力的仓库。
func (s *Server) issueRepos(write bool) []*Repo {
	var out []*Repo
	for _, r := range s.store.Repos() {
		if !r.IssueRead || (write && !r.IssueWrite) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func issueRepoList(rs []*Repo) string {
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		names = append(names, r.Name)
	}
	if len(names) == 0 {
		return "（无）"
	}
	return strings.Join(names, " / ")
}

// issueMode 是单仓 issue 能力的机器可读形态，供 /healthz 使用。
func issueMode(r *Repo) string {
	switch {
	case r.IssueWrite:
		return "write"
	case r.IssueRead:
		return "read"
	default:
		return "off"
	}
}

// resolveIssueRepo 在开启了 issue 能力的仓库里解析 repo 参数。
// 只有唯一候选时才允许省略：把 issue 提错仓库比不提更糟，这里绝不猜。
func (s *Server) resolveIssueRepo(args map[string]any, write bool) (*Repo, error) {
	cands := s.issueRepos(write)
	if len(cands) == 0 {
		return nil, fmt.Errorf("当前没有任何仓库开启 issue %s能力", map[bool]string{true: "写入", false: "检索"}[write])
	}
	name := strings.ToLower(argStr(args, "repo"))
	if name == "" {
		if len(cands) == 1 {
			return cands[0], nil
		}
		return nil, fmt.Errorf("必须指定 repo，可选：%s", issueRepoList(cands))
	}
	for _, r := range cands {
		if r.Name == name {
			return r, nil
		}
	}
	// 命中了仓库但能力不够时说清原因，否则模型会换个仓库重试——那正是最该避免的事。
	if r, ok := s.store.Get(name); ok {
		if !r.IssueRead {
			return nil, fmt.Errorf("仓库 %s 未接入 issue，问题请引导用户到该项目的仓库页面反馈。可用：%s", name, issueRepoList(cands))
		}
		return nil, fmt.Errorf("仓库 %s 的 issue 是只读的，本服务无权写入。可写：%s", name, issueRepoList(cands))
	}
	return nil, fmt.Errorf("未知仓库 %q，可用：%s", name, issueRepoList(cands))
}

// ── 工具定义 ────────────────────────────────────────────────

// issueToolDefs 按配置动态产出 issue 工具：没开写能力就根本不暴露 create/update。
// 工具不存在比工具存在但被拒绝更有效——模型看不见的能力不会去尝试。
func (s *Server) issueToolDefs() []toolDef {
	readable := s.issueRepos(false)
	if len(readable) == 0 {
		return nil
	}
	writable := s.issueRepos(true)

	repoDesc := "仓库短名，取值：" + issueRepoList(readable)
	if len(readable) == 1 {
		repoDesc += "（只有一个，可省略）"
	}

	defs := []toolDef{
		{
			Name:  "search_issues",
			Title: "检索 issue",
			Desc: "检索仓库里已有的 issue（不含 PR）。两个用途：回答「这个问题有没有人提过 / 现在有哪些待办 / 某功能什么状态」；" +
				"以及在 create_issue 之前查重。重复提 issue 会污染仓库，提交前必须先查。" +
				"关键词用现象里的核心名词，中英文都可以，不要把用户整句话丢进来。" +
				"每条 issue 状态直接标注：[开放中] / [已关闭] / [closed/已解决] / [closed/不予处理]，按标注如实回答，不要自行推断。",
			Schema: obj(map[string]any{
				"query":  str("关键词，如 下载 断点续传 / aria2 timeout；省略则按更新时间列出最近的"),
				"repo":   str(repoDesc),
				"state":  str("状态过滤：open（默认）/ closed / all。查重时用 all"),
				"labels": str("标签过滤，多个用逗号分隔"),
				"limit":  integer("返回条数，默认 10", 1, 30),
			}),
			Handle: s.toolSearchIssues,
		},
		{
			Name:  "read_issue",
			Title: "读取 issue",
			Desc: "按编号读取单个 issue 的完整正文与最近的讨论。" +
				"在 search_issues 拿到候选后，需要判断「是不是同一个问题」「维护者怎么答复的」时用它。",
			Schema: obj(map[string]any{
				"number": integer("issue 编号（不带 #）", 1, 1000000),
				"repo":   str(repoDesc),
			}, "number"),
			Handle: s.toolReadIssue,
		},
	}

	if len(writable) == 0 {
		return defs
	}

	writeDesc := "仓库短名，取值：" + issueRepoList(writable)
	if len(writable) == 1 {
		writeDesc += "（只有一个，可省略）"
	}

	return append(defs,
		toolDef{
			Name:  "create_issue",
			Title: "创建 issue",
			Desc: "在仓库里创建一个新 issue。这是真实写入，别人会收到通知，必须同时满足以下条件才可调用：\n" +
				"1) 用户报告的是缺陷、异常或功能需求——单纯的用法提问、你查代码就能答的问题，直接回答，不要开 issue；\n" +
				"2) 你已经用 search_code / find_symbol / read_file 对问题做过调研（无论是否查到，结论都要写进 evidence）；\n" +
				"3) 你已经用 search_issues（state=all）查过重，确认没有相同问题；\n" +
				"4) 问题确实属于这个仓库——不属于任何已接入仓库的问题不要硬提。\n" +
				"服务端会再做一次自动查重并限制创建频率；命中疑似重复会拒绝创建并列出候选。\n" +
				"创建成功后把编号和链接告诉用户，同一问题不要再提第二次。",
			Schema: obj(map[string]any{
				"title": str("一句话标题：用户视角描述现象，不要写成「修复 xxx」。示例：多任务并发时进度条偶发不刷新"),
				"body":  str("问题描述：用户做了什么、期望什么、实际发生什么。原样保留用户给出的报错文本"),
				"confidence": str("调研结论的确定性，二选一：" +
					"confirmed=已在源码中定位到相关实现且能给出出处；unconfirmed=没能定位，需要维护者核实。不确定就填 unconfirmed，不要硬凑"),
				"evidence": str("调研过程与依据。confirmed 时写出 路径:行号 及你的判断；" +
					"unconfirmed 时写清你检索了哪些关键词、看了哪些文件、为什么无法确认"),
				"repro":                 str("复现步骤或触发条件（缺陷类强烈建议填写）"),
				"env":                   str("用户报告的版本 / 系统 / 环境信息"),
				"reporter":              str("报告人标识（IM 昵称等），会写进 issue 正文以便追溯"),
				"labels":                str("标签，多个用逗号分隔。只有仓库已存在的标签会被采用，其余自动忽略"),
				"repo":                  str(writeDesc),
				"confirm_not_duplicate": boolean("仅在服务端查重拒绝、且你逐条读过候选确认都不是同一问题后才置 true"),
			}, "title", "body", "confidence", "evidence"),
			Handle: s.toolCreateIssue,
		},
		toolDef{
			Name:  "update_issue",
			Title: "管理 issue",
			Desc: "对已有 issue 追加评论、关闭、重新打开或增删标签。真实写入，规则：\n" +
				"- 补充信息一律用 comment，不要为同一问题另开 issue；\n" +
				"- 只有在用户明确要求，或问题确已解决/确认不做时才 close，且必须用 comment 写清结论；\n" +
				"- 不要为了「清理」而批量关闭，一次只处理一个编号；\n" +
				"- 不确定是否该关时，改为 comment 说明情况，把决定权留给维护者。",
			Schema: obj(map[string]any{
				"number":        integer("issue 编号（不带 #）", 1, 1000000),
				"action":        str("comment（默认，仅评论）/ close（关闭）/ reopen（重新打开）"),
				"comment":       str("要追加的评论。close 与 reopen 必填，需说明结论或理由"),
				"reason":        str("关闭原因，close 时必填：completed=已解决 / not_planned=不予处理或无法复现"),
				"add_labels":    str("要添加的标签，逗号分隔；只有仓库已存在的标签会被采用"),
				"remove_labels": str("要移除的标签，逗号分隔"),
				"repo":          str(writeDesc),
			}, "number"),
			Handle: s.toolUpdateIssue,
		},
	)
}

// ── search_issues ──────────────────────────────────────────

func (s *Server) toolSearchIssues(ctx context.Context, args map[string]any) (string, error) {
	r, err := s.resolveIssueRepo(args, false)
	if err != nil {
		return "", err
	}
	q := IssueQuery{
		Text:   argStr(args, "query"),
		State:  argStr(args, "state"),
		Labels: splitList(argStr(args, "labels")),
		Limit:  argInt(args, "limit", 10),
	}
	items, err := s.gh.List(ctx, r, q)
	if err != nil {
		return "", err
	}

	state := ghNormState(q.State)
	w := newBudget(s.cfg.MaxResponseBytes)
	scope := fmt.Sprintf("%s（状态 %s）", r.Slug, state)
	if q.Text != "" {
		scope = fmt.Sprintf("%s 中检索 %q（状态 %s）", r.Slug, q.Text, state)
	}
	if len(items) == 0 {
		w.line("未找到匹配的 issue：" + scope + "。")
		if state == "open" {
			w.line("提示：查重时用 state=all，已关闭的 issue 里可能已有结论。")
		}
		w.line("若这是用户新报告的缺陷或需求：先用 search_code / find_symbol 调研，再用 create_issue 提交（无法定位也要如实写明）。")
		return w.String(), nil
	}

	w.line(fmt.Sprintf("%s，命中 %d 条：", scope, len(items)))
	for _, it := range items {
		w.line("")
		if !w.line(fmt.Sprintf("#%d [%s] %s", it.Number, issueStateText(it), truncate(it.Title, 160))) {
			break
		}
		w.line("    " + issueMetaLine(it))
		if it.URL != "" {
			w.line("    " + it.URL)
		}
		for _, l := range issueSummary(it.Body, 2, 160) {
			if !w.line("    " + l) {
				break
			}
		}
	}
	w.line("")
	w.line("要看完整正文与讨论，用 read_issue + 编号。")
	return w.String(), nil
}

// ── read_issue ─────────────────────────────────────────────

func (s *Server) toolReadIssue(ctx context.Context, args map[string]any) (string, error) {
	r, err := s.resolveIssueRepo(args, false)
	if err != nil {
		return "", err
	}
	number := argInt(args, "number", 0)
	if number <= 0 {
		return "", fmt.Errorf("number 必须是正整数（issue 编号）")
	}
	iss, comments, err := s.gh.Get(ctx, r, number)
	if err != nil {
		return "", err
	}

	w := newBudget(s.cfg.MaxResponseBytes)
	w.line(fmt.Sprintf("#%d [%s] %s", iss.Number, issueStateText(iss), iss.Title))
	w.line(issueMetaLine(iss))
	if iss.URL != "" {
		w.line(iss.URL)
	}
	w.line("")
	body := strings.TrimSpace(iss.Body)
	if body == "" {
		w.line("（正文为空）")
	} else {
		for _, l := range strings.Split(body, "\n") {
			if !w.line(truncate(l, 300)) {
				break
			}
		}
	}
	if len(comments) == 0 {
		w.line("")
		w.line("暂无评论。")
		return w.String(), nil
	}
	w.line("")
	w.line(fmt.Sprintf("── 讨论（最近 %d 条）──", len(comments)))
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

// ── create_issue ───────────────────────────────────────────

func (s *Server) toolCreateIssue(ctx context.Context, args map[string]any) (string, error) {
	r, err := s.resolveIssueRepo(args, true)
	if err != nil {
		return "", err
	}

	title := strings.TrimSpace(argStr(args, "title"))
	body := strings.TrimSpace(argStr(args, "body"))
	evidence := strings.TrimSpace(argStr(args, "evidence"))
	confidence := strings.ToLower(strings.TrimSpace(argStr(args, "confidence")))

	if n := utf8.RuneCountInString(title); n < issueTitleMinRunes || n > issueTitleMaxRunes {
		return "", fmt.Errorf("title 长度需在 %d–%d 字之间，当前 %d 字", issueTitleMinRunes, issueTitleMaxRunes, n)
	}
	if utf8.RuneCountInString(body) < issueBodyMinRunes {
		return "", fmt.Errorf("body 太短（不足 %d 字）：需写清用户做了什么、期望什么、实际发生什么", issueBodyMinRunes)
	}
	var confirmed bool
	switch confidence {
	case "confirmed", "yes", "true":
		confirmed = true
	case "unconfirmed", "no", "false":
		confirmed = false
	default:
		return "", fmt.Errorf("confidence 必须是 confirmed 或 unconfirmed，当前 %q", argStr(args, "confidence"))
	}
	if utf8.RuneCountInString(evidence) < issueEvidMinRunes {
		if confirmed {
			return "", fmt.Errorf("confidence=confirmed 时 evidence 必须给出 路径:行号 与判断依据；调研不足请先用 search_code / find_symbol 再来")
		}
		return "", fmt.Errorf("evidence 不能省略：即使未能定位，也要写明检索过哪些关键词、看过哪些文件、为什么无法确认")
	}
	if confirmed && !strings.Contains(evidence, ":") && !strings.Contains(evidence, "：") {
		return "", fmt.Errorf("confidence=confirmed 但 evidence 里没有 路径:行号 形式的出处；若确实定位不到请改用 unconfirmed")
	}

	// 服务端强制查重：模型自称查过不算数。
	if !argBool(args, "confirm_not_duplicate") {
		if dups := s.findIssueDuplicates(ctx, r, title); len(dups) > 0 {
			w := newBudget(s.cfg.MaxResponseBytes)
			w.line("未创建：" + r.Slug + " 中已有疑似重复的 issue。")
			for _, d := range dups {
				w.line("")
				w.line(fmt.Sprintf("#%d [%s] %s", d.Number, issueStateText(d), truncate(d.Title, 160)))
				if d.URL != "" {
					w.line("    " + d.URL)
				}
			}
			w.line("")
			w.line("请先用 read_issue 逐条核对：")
			w.line("  - 是同一个问题 → 不要新建，把该 issue 的编号、状态与结论回复用户；有新信息就用 update_issue 追加评论。")
			w.line("  - 确认都不是同一问题 → 带 confirm_not_duplicate=true 重新调用 create_issue。")
			return w.String(), nil
		}
	}

	if ok, wait := s.limiter.take(r.Name); !ok {
		return "", fmt.Errorf("未创建：%s 每小时最多创建 %d 个 issue，已达上限，约 %d 分钟后可再试。"+
			"请先把已有 issue 的链接回复用户，或用 update_issue 在既有 issue 下补充", r.Name, s.cfg.issueLimit, int(wait.Minutes())+1)
	}

	labels, dropped := s.pickLabels(ctx, r, splitList(argStr(args, "labels")))
	draft := IssueDraft{
		Title:  title,
		Body:   s.renderIssueBody(r, body, evidence, confirmed, argStr(args, "repro"), argStr(args, "env"), argStr(args, "reporter")),
		Labels: labels,
	}
	iss, err := s.gh.Create(ctx, r, draft)
	if err != nil {
		return "", err
	}

	w := newBudget(s.cfg.MaxResponseBytes)
	w.line(fmt.Sprintf("已创建 issue #%d：%s", iss.Number, iss.Title))
	if iss.URL != "" {
		w.line(iss.URL)
	}
	if len(labels) > 0 {
		w.line("标签：" + strings.Join(labels, ", "))
	}
	if len(dropped) > 0 {
		w.line("已忽略仓库中不存在的标签：" + strings.Join(dropped, ", "))
	}
	if !confirmed {
		w.line("已标注为「未能在源码中确认，需维护者核实」。")
	}
	if rem := s.limiter.remaining(r.Name); rem >= 0 {
		w.line(fmt.Sprintf("本小时该仓还可创建 %d 个 issue。", rem))
	}
	w.line("")
	w.line("请把编号与链接告诉用户，并说明维护者会在仓库里跟进；同一问题不要再次创建。")
	return w.String(), nil
}

// findIssueDuplicates 双路召回后按标题相似度筛出疑似重复：
// 搜索接口覆盖历史 issue，最近 open 列表兜底中文标题（GitHub 搜索对 CJK 分词很差）。
// 任何一路失败都不阻断创建——查重是尽力而为，不能因为 GitHub 抖动就让用户提不了问题。
func (s *Server) findIssueDuplicates(ctx context.Context, r *Repo, title string) []Issue {
	seen := make(map[int]bool)
	var pool []Issue
	add := func(items []Issue) {
		for _, it := range items {
			if !seen[it.Number] {
				seen[it.Number] = true
				pool = append(pool, it)
			}
		}
	}
	if items, err := s.gh.List(ctx, r, IssueQuery{Text: title, State: "all", Limit: 20}); err == nil {
		add(items)
	}
	if items, err := s.gh.List(ctx, r, IssueQuery{State: "open", Limit: 50}); err == nil {
		add(items)
	}

	qt := ghTextTokens(title)
	type scored struct {
		iss   Issue
		score float64
	}
	var ranked []scored
	for _, it := range pool {
		if sc := ghSimilarity(qt, ghTextTokens(it.Title)); sc >= issueDupThreshold {
			ranked = append(ranked, scored{it, sc})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if len(ranked) > issueDupMax {
		ranked = ranked[:issueDupMax]
	}
	out := make([]Issue, 0, len(ranked))
	for _, x := range ranked {
		out = append(out, x.iss)
	}
	return out
}

// renderIssueBody 由服务端拼装正文。模型只能填各段内容，不能控制整体结构，
// 这样每一条机器人提交的 issue 都必然带上确定性标注与来源说明。
func (s *Server) renderIssueBody(r *Repo, body, evidence string, confirmed bool, repro, env, reporter string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(body))
	b.WriteString("\n\n## 调研结论\n\n")
	if confirmed {
		b.WriteString("**已在源码中定位到相关实现**\n\n")
	} else {
		b.WriteString("**未能在源码中确认，需维护者核实**\n\n")
	}
	b.WriteString(strings.TrimSpace(evidence))
	b.WriteString("\n")
	if v := strings.TrimSpace(repro); v != "" {
		b.WriteString("\n## 复现 / 触发条件\n\n" + v + "\n")
	}
	if v := strings.TrimSpace(env); v != "" {
		b.WriteString("\n## 环境\n\n" + v + "\n")
	}

	who := strings.TrimSpace(reporter)
	if who == "" {
		who = "IM 用户"
	}
	b.WriteString("\n---\n\n")
	b.WriteString(fmt.Sprintf("由聊天机器人代 **%s** 提交", truncate(who, 60)))
	if head := s.shortHead(r.Name); head != "" {
		b.WriteString(fmt.Sprintf("（repoMcp，索引 commit `%s`）", head))
	}
	b.WriteString("。代码定位来自自动检索，可能不完整，请以实际代码为准。\n")
	return b.String()
}

// ── update_issue ───────────────────────────────────────────

func (s *Server) toolUpdateIssue(ctx context.Context, args map[string]any) (string, error) {
	r, err := s.resolveIssueRepo(args, true)
	if err != nil {
		return "", err
	}
	number := argInt(args, "number", 0)
	if number <= 0 {
		return "", fmt.Errorf("number 必须是正整数（issue 编号）")
	}

	action := strings.ToLower(strings.TrimSpace(argStr(args, "action")))
	if action == "" {
		action = "comment"
	}
	comment := strings.TrimSpace(argStr(args, "comment"))
	addLabels := splitList(argStr(args, "add_labels"))
	rmLabels := splitList(argStr(args, "remove_labels"))

	var edit IssueEdit
	switch action {
	case "comment":
		if comment == "" && len(addLabels) == 0 && len(rmLabels) == 0 {
			return "", fmt.Errorf("action=comment 时至少要给出 comment 或标签变更")
		}
	case "close":
		if utf8.RuneCountInString(comment) < issueNoteMinRunes {
			return "", fmt.Errorf("关闭 issue 必须在 comment 里写清结论（已修复 / 无法复现 / 不予处理及原因），至少 %d 字", issueNoteMinRunes)
		}
		switch strings.ToLower(strings.TrimSpace(argStr(args, "reason"))) {
		case "completed", "done", "fixed":
			edit.StateReason = "completed"
		case "not_planned", "notplanned", "wontfix", "invalid":
			edit.StateReason = "not_planned"
		default:
			return "", fmt.Errorf("close 需要 reason：completed（已解决）或 not_planned（不予处理 / 无法复现）")
		}
		edit.State = "closed"
	case "reopen", "open":
		if utf8.RuneCountInString(comment) < issueNoteMinRunes {
			return "", fmt.Errorf("重新打开 issue 必须在 comment 里说明理由，至少 %d 字", issueNoteMinRunes)
		}
		edit.State = "open"
	default:
		return "", fmt.Errorf("未知 action %q，可选：comment / close / reopen", action)
	}

	// 先读当前状态：重复关闭一个已关闭的 issue 只会制造噪声通知。
	cur, _, err := s.gh.Get(ctx, r, number)
	if err != nil {
		return "", err
	}
	if edit.State == "closed" && cur.State == "closed" {
		return "", fmt.Errorf("#%d 已经是关闭状态（%s），无需重复关闭。若要补充信息请用 action=comment", number, issueStateText(cur))
	}
	if edit.State == "open" && cur.State == "open" {
		return "", fmt.Errorf("#%d 当前就是开启状态，无需重新打开", number)
	}

	var dropped []string
	if len(addLabels) > 0 {
		addLabels, dropped = s.pickLabels(ctx, r, addLabels)
	}
	edit.AddLabels = addLabels
	edit.RemoveLabels = rmLabels

	// 评论先发：状态变更会触发通知，通知里带上结论比事后补评论更清楚。
	if comment != "" {
		if err := s.gh.Comment(ctx, r, number, s.renderComment(comment)); err != nil {
			return "", err
		}
	}
	iss := cur
	if edit.State != "" || len(edit.AddLabels) > 0 || len(edit.RemoveLabels) > 0 {
		iss, err = s.gh.Edit(ctx, r, number, edit)
		if err != nil {
			return "", fmt.Errorf("评论已提交，但后续修改失败：%w", err)
		}
	}

	w := newBudget(s.cfg.MaxResponseBytes)
	switch {
	case edit.State == "closed":
		w.line(fmt.Sprintf("已关闭 #%d（%s）：%s", iss.Number, edit.StateReason, truncate(iss.Title, 120)))
	case edit.State == "open":
		w.line(fmt.Sprintf("已重新打开 #%d：%s", iss.Number, truncate(iss.Title, 120)))
	default:
		w.line(fmt.Sprintf("已更新 #%d：%s", iss.Number, truncate(iss.Title, 120)))
	}
	if comment != "" {
		w.line("已追加评论。")
	}
	if len(edit.AddLabels) > 0 {
		w.line("已添加标签：" + strings.Join(edit.AddLabels, ", "))
	}
	if len(edit.RemoveLabels) > 0 {
		w.line("已移除标签：" + strings.Join(edit.RemoveLabels, ", "))
	}
	if len(dropped) > 0 {
		w.line("已忽略仓库中不存在的标签：" + strings.Join(dropped, ", "))
	}
	if iss.URL != "" {
		w.line(iss.URL)
	}
	return w.String(), nil
}

// renderComment 给评论加上来源标注：仓库里必须能一眼看出哪些内容是机器人写的。
func (s *Server) renderComment(body string) string {
	return strings.TrimSpace(body) + "\n\n<sub>— 由聊天机器人经 repoMcp 提交</sub>\n"
}

// ── 公共辅助 ────────────────────────────────────────────────

// pickLabels 过滤模型给出的标签，只保留可用的。
// 必须过滤：GitHub 打标签时会顺手新建不存在的标签，不校验就等于让机器人改仓库的标签体系。
func (s *Server) pickLabels(ctx context.Context, r *Repo, want []string) (keep, dropped []string) {
	if len(want) == 0 {
		return nil, nil
	}
	allowed := r.IssueLabels
	if len(allowed) == 0 {
		names, err := s.gh.RepoLabels(ctx, r)
		if err != nil {
			// 核对不了就一个都不用：宁可少个标签，也不要造出新标签。
			return nil, want
		}
		allowed = names
	}
	index := make(map[string]string, len(allowed))
	for _, a := range allowed {
		index[strings.ToLower(a)] = a
	}
	for _, wnt := range want {
		if real, ok := index[strings.ToLower(wnt)]; ok {
			keep = append(keep, real)
		} else {
			dropped = append(dropped, wnt)
		}
	}
	return keep, dropped
}

// splitList 按逗号（中英文）与换行拆分列表参数，并去重去空。
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == ';' || r == '；'
	})
	seen := make(map[string]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out
}

func issueStateText(it Issue) string {
	switch it.State {
	case "closed":
		switch it.Reason {
		case "completed":
			return "closed/已解决"
		case "not_planned":
			return "closed/不予处理"
		}
		return "已关闭"
	default:
		return "开放中"
	}
}

func issueMetaLine(it Issue) string {
	parts := make([]string, 0, 5)
	if len(it.Labels) > 0 {
		parts = append(parts, "标签 "+strings.Join(it.Labels, ","))
	}
	parts = append(parts, fmt.Sprintf("%d 评论", it.Comments))
	if it.CreatedAt != "" {
		parts = append(parts, "创建 "+it.CreatedAt)
	}
	if it.UpdatedAt != "" && it.UpdatedAt != it.CreatedAt {
		parts = append(parts, "更新 "+it.UpdatedAt)
	}
	if it.Author != "" {
		parts = append(parts, "作者 "+it.Author)
	}
	return strings.Join(parts, " · ")
}

// issueSummary 取正文的前若干行有效内容，跳过 Markdown 标题与引用行。
func issueSummary(body string, maxLines, width int) []string {
	out := make([]string, 0, maxLines)
	for _, l := range strings.Split(body, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ">") || strings.HasPrefix(t, "---") {
			continue
		}
		out = append(out, truncate(t, width))
		if len(out) >= maxLines {
			break
		}
	}
	return out
}
