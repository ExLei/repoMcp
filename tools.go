// 工具层：暴露给 LLM 的 5 个工具及其输出格式。
//
// 输出刻意是紧凑纯文本而非 JSON：消费方是 IM 里的小模型，JSON 包装只增加 token
// 却不提升可读性。每条结果都带 路径:行号 与 permalink，保证答案可被人工核验。
// 所有输出都过字节预算（cfg.MaxResponseBytes），因为 LangBot 不截断 tool 返回。
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var errUnknownTool = errors.New("unknown tool")

type toolDef struct {
	Name   string
	Title  string
	Desc   string
	Schema map[string]any
	Handle func(ctx context.Context, args map[string]any) (string, error)
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": min, "maximum": max}
}
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func (s *Server) toolDefs() []toolDef {
	repoDesc := "仓库短名，取值：" + strings.Join(s.repoNames(), " / ")
	if len(s.store.Repos()) == 1 {
		repoDesc += "（只有一个仓库，可省略）"
	}

	defs := []toolDef{
		{
			Name:  "repo_overview",
			Title: "仓库概览",
			Desc: "列出已索引仓库的技术栈、规模、目录结构与 README 摘要。" +
				"在不清楚代码放在哪、或不确定该用什么关键词检索时，先调用它建立坐标，再用 search_code。",
			Schema: obj(map[string]any{
				"repo": str(repoDesc + "；省略则概述全部仓库"),
			}),
			Handle: s.toolRepoOverview,
		},
		{
			Name:  "search_code",
			Title: "检索源码",
			Desc: "在源码中做关键词检索，返回带行号与链接的代码片段。" +
				"查询词用代码里真实会出现的标识符、报错文本或英文术语（如 retry backoff、connection pool），" +
				"不要用整句中文提问。已知确切的函数或类型名时改用 find_symbol 更准。",
			Schema: obj(map[string]any{
				"query":     str("检索词，空格分隔的关键词或一段原文子串"),
				"repo":      str(repoDesc + "；省略则搜全部"),
				"lang":      str("按语言过滤，如 go / rust / dart / typescript / python"),
				"path_glob": str("按路径过滤的通配符，如 src/**/*.rs 或 *_test.go"),
				"k":         integer("返回条数，默认 8", 1, 30),
			}, "query"),
			Handle: s.toolSearchCode,
		},
		{
			Name:  "read_file",
			Title: "读取文件",
			Desc: "按行范围读取某个文件的原文。用于看清 search_code / find_symbol 命中处的完整实现。" +
				"必须先通过检索拿到确切路径再调用；一次最多读 400 行。",
			Schema: obj(map[string]any{
				"path":  str("仓库内文件路径，如 native/engine/src/lib.rs；也接受唯一可确定的路径后缀"),
				"repo":  str(repoDesc),
				"start": integer("起始行，1-based，默认 1", 1, 1000000),
				"end":   integer("结束行（含）。省略则从 start 起读 120 行", 1, 1000000),
				"blame": boolean("为每行附加最后修改的提交与作者，默认 false"),
			}, "path"),
			Handle: s.toolReadFile,
		},
		{
			Name:  "find_symbol",
			Title: "查找定义",
			Desc: "按名字查找函数、方法、类型、类、接口、常量的定义位置，返回签名与文档注释。" +
				"回答「X 在哪里定义 / X 是什么 / X 的签名」这类问题时首选它，比 search_code 精确。",
			Schema: obj(map[string]any{
				"name": str("符号名，支持前缀与部分匹配，忽略大小写"),
				"repo": str(repoDesc + "；省略则搜全部"),
				"kind": str("按种类过滤：func / method / type / struct / class / interface / trait / enum / const / var"),
				"k":    integer("返回条数，默认 20", 1, 100),
			}, "name"),
			Handle: s.toolFindSymbol,
		},
		{
			Name:  "git_history",
			Title: "提交历史",
			Desc: "查询提交记录，用于回答「这段代码为什么这么写」「这个功能什么时候加的」「最近改了什么」。" +
				"可按文件路径限定，或用关键词搜提交信息。",
			Schema: obj(map[string]any{
				"repo":  str(repoDesc),
				"path":  str("只看这个文件或目录的历史"),
				"query": str("在提交信息中搜索的关键词"),
				"limit": integer("返回条数，默认 10", 1, 50),
			}),
			Handle: s.toolGitHistory,
		},
	}
	// issue 工具按配置动态挂载：没接入 issue 的部署仍然只有这 5 个检索工具。
	return append(append(defs, s.issueToolDefs()...), s.pullToolDefs()...)
}

func (s *Server) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	for _, d := range s.toolDefs() {
		if d.Name == name {
			if args == nil {
				args = map[string]any{}
			}
			out, err := d.Handle(ctx, args)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(out) == "" {
				return "（无结果）", nil
			}
			return out, nil
		}
	}
	return "", fmt.Errorf("%w: %s", errUnknownTool, name)
}

// ── 参数读取 ────────────────────────────────────────────────

func argStr(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// argInt 容忍模型把数字写成字符串或浮点，这在小模型上很常见。
func argInt(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return def
}

func argBool(args map[string]any, key string) bool {
	v, ok := args[key]
	if !ok || v == nil {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(strings.TrimSpace(b), "true")
	}
	return false
}

func (s *Server) repoNames() []string {
	rs := s.store.Repos()
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

// resolveRepo 解析 repo 参数。单仓部署时允许省略，这能显著降低小模型的出错率。
func (s *Server) resolveRepo(args map[string]any, required bool) (*Repo, error) {
	name := strings.ToLower(argStr(args, "repo"))
	all := s.store.Repos()
	if name == "" {
		if len(all) == 1 {
			return all[0], nil
		}
		if !required {
			return nil, nil
		}
		return nil, fmt.Errorf("需要指定 repo，可选：%s", strings.Join(s.repoNames(), " / "))
	}
	if r, ok := s.store.Get(name); ok {
		return r, nil
	}
	return nil, fmt.Errorf("未知仓库 %q，可选：%s", name, strings.Join(s.repoNames(), " / "))
}

// ── 输出预算 ────────────────────────────────────────────────

// budget 是带字节上限的行缓冲。超限后停止收字并在末尾说明被裁剪，
// 这样模型知道"还有更多"，而不是误以为结果已穷尽。
type budget struct {
	b       strings.Builder
	max     int
	dropped int
}

// budgetNoticeReserve 为末尾的裁剪说明预留字节，
// 否则超限时追加说明会让总输出反过来越过预算。
const budgetNoticeReserve = 220

func newBudget(max int) *budget {
	m := max - budgetNoticeReserve
	if m < 500 {
		m = 500
	}
	return &budget{max: m}
}

// line 追加一行；返回 false 表示预算已尽，调用方应停止产出。
func (w *budget) line(s string) bool {
	if w.b.Len()+len(s)+1 > w.max {
		w.dropped++
		return false
	}
	w.b.WriteString(s)
	w.b.WriteByte('\n')
	return true
}

func (w *budget) String() string {
	out := w.b.String()
	if w.dropped > 0 {
		out += fmt.Sprintf("\n…输出达到长度上限，已省略 %d 行。缩小检索范围（指定 repo / path_glob / 更具体的关键词）可看到其余内容。\n", w.dropped)
	}
	return out
}

// link 生成可核验的 permalink。sha 钉住行号，避免分支推进后链接错位。
func (s *Server) link(repo, path string, start, end int) string {
	r, ok := s.store.Get(repo)
	if !ok || r.WebBase == "" {
		return ""
	}
	sha := s.store.Head(repo)
	if sha == "" {
		return ""
	}
	u := r.WebBase + "/blob/" + sha + "/" + path
	if start > 0 {
		u += "#L" + itoa(start)
		if end > start {
			u += "-L" + itoa(end)
		}
	}
	return u
}

func (s *Server) shortHead(repo string) string {
	h := s.store.Head(repo)
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// notReady 在索引尚未建立时给出可操作的说明，而不是空结果。
func (s *Server) notReady(repos []*Repo) string {
	stats := s.index.Stats()
	var pending []string
	for _, r := range repos {
		if _, ok := stats[r.Name]; !ok {
			msg := r.Name
			if _, _, err := s.store.Status(r.Name); err != nil {
				msg += "（同步失败：" + truncate(err.Error(), 160) + "）"
			} else {
				msg += "（首次克隆与索引进行中）"
			}
			pending = append(pending, msg)
		}
	}
	if len(pending) == 0 {
		return ""
	}
	return "以下仓库尚不可检索：" + strings.Join(pending, "、") + "。请稍后重试。"
}

// ── repo_overview ──────────────────────────────────────────

func (s *Server) toolRepoOverview(_ context.Context, args map[string]any) (string, error) {
	target, err := s.resolveRepo(args, false)
	if err != nil {
		return "", err
	}
	repos := s.store.Repos()
	if target != nil {
		repos = []*Repo{target}
	}
	stats := s.index.Stats()
	w := newBudget(s.cfg.MaxResponseBytes)

	// 单仓时给出更深的结构信息；多仓时每仓只给摘要，留出预算覆盖所有仓库。
	detailed := len(repos) == 1
	treeLimit := 12
	if detailed {
		treeLimit = 40
	}

	for i, r := range repos {
		if i > 0 && !w.line("") {
			break
		}
		st, indexed := stats[r.Name]
		head := s.shortHead(r.Name)
		title := "## " + r.Name
		if head != "" {
			title += " @" + head + " (" + r.Ref + ")"
		}
		if !w.line(title) {
			break
		}
		if d := s.cfg.desc(r.Name); d != "" {
			w.line(d)
		}
		if !indexed {
			w.line("状态：尚未索引完成。" + s.notReady([]*Repo{r}))
			continue
		}
		w.line(fmt.Sprintf("规模：%d 个文件 / %d 行 / %d 个符号", st.Files, st.Lines, st.Symbols))
		if langs := topLangs(st.ByLang, 8); langs != "" {
			w.line("技术栈：" + langs)
		}
		if r.WebBase != "" {
			w.line("仓库地址：" + r.WebBase)
		}
		if r.IssueRead {
			mode := "可检索（search_issues / read_issue）"
			if r.IssueWrite {
				mode = "可检索，也可代用户提交与管理（create_issue / update_issue，需先调研并查重）"
			}
			w.line("issue：" + mode + " —— " + r.Slug)
		}

		if lines := s.index.Tree(r.Name, treeLimit); len(lines) > 0 {
			w.line("目录结构：")
			for _, l := range lines {
				if !w.line("  " + l) {
					break
				}
			}
		}
		if detailed {
			if head := s.readmeHead(r.Name, 25); head != "" {
				w.line("README 摘要：")
				for _, l := range strings.Split(head, "\n") {
					if !w.line("  " + l) {
						break
					}
				}
			}
		}
	}
	if len(repos) > 1 {
		w.line("")
		w.line("提示：对单个仓库调用 repo_overview 可获得更详细的结构与 README。")
	}
	return w.String(), nil
}

func topLangs(byLang map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	items := make([]kv, 0, len(byLang))
	for k, v := range byLang {
		if k == "" {
			continue
		}
		items = append(items, kv{k, v})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].v != items[j].v {
			return items[i].v > items[j].v
		}
		return items[i].k < items[j].k
	})
	if len(items) > n {
		items = items[:n]
	}
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, fmt.Sprintf("%s(%d)", it.k, it.v))
	}
	return strings.Join(parts, " ")
}

// readmeHead 取 README 的前若干有效行，跳过徽章与空行。
func (s *Server) readmeHead(repo string, maxLines int) string {
	for _, name := range []string{"README.md", "readme.md", "README.MD", "README", "README.rst", "docs/README.md"} {
		f, ok := s.index.File(repo, name)
		if !ok {
			continue
		}
		out := make([]string, 0, maxLines)
		for _, l := range f.Lines {
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "[!") || strings.HasPrefix(t, "<img") || strings.HasPrefix(t, "<p align") {
				continue
			}
			out = append(out, truncate(t, 160))
			if len(out) >= maxLines {
				break
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}

// ── search_code ────────────────────────────────────────────

func (s *Server) toolSearchCode(_ context.Context, args map[string]any) (string, error) {
	query := argStr(args, "query")
	if query == "" {
		return "", errors.New("query 不能为空")
	}
	target, err := s.resolveRepo(args, false)
	if err != nil {
		return "", err
	}
	scope := s.store.Repos()
	repoName := ""
	if target != nil {
		scope = []*Repo{target}
		repoName = target.Name
	}
	if msg := s.notReady(scope); msg != "" && len(s.index.Stats()) == 0 {
		return msg, nil
	}

	k := argInt(args, "k", 8)
	hits := s.index.Search(SearchQuery{
		Text:     query,
		Repo:     repoName,
		Lang:     strings.ToLower(argStr(args, "lang")),
		PathGlob: argStr(args, "path_glob"),
		K:        k,
	})

	w := newBudget(s.cfg.MaxResponseBytes)
	if len(hits) == 0 {
		w.line(fmt.Sprintf("未找到与 %q 匹配的代码。", query))
		w.line("建议：改用代码中实际出现的英文标识符；放宽 lang / path_glob 过滤；或先用 repo_overview 了解结构。")
		if msg := s.notReady(scope); msg != "" {
			w.line(msg)
		}
		return w.String(), nil
	}

	head := fmt.Sprintf("检索 %q，命中 %d 条", query, len(hits))
	if repoName != "" {
		head += "（仓库 " + repoName + " @" + s.shortHead(repoName) + "）"
	}
	w.line(head)

	for i, h := range hits {
		w.line("")
		loc := fmt.Sprintf("[%d] %s:%d", i+1, h.Path, h.Line)
		if len(scope) > 1 || repoName == "" {
			loc = fmt.Sprintf("[%d] %s/%s:%d", i+1, h.Repo, h.Path, h.Line)
		}
		if h.Why != "" {
			loc += "  (" + h.Why + ")"
		}
		if !w.line(loc) {
			break
		}
		if u := s.link(h.Repo, h.Path, h.Line, h.EndLine); u != "" {
			w.line("    " + u)
		}
		stop := false
		for _, l := range strings.Split(strings.TrimRight(h.Snippet, "\n"), "\n") {
			if !w.line(l) {
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

// ── read_file ──────────────────────────────────────────────

const readFileMaxLines = 400

func (s *Server) toolReadFile(ctx context.Context, args map[string]any) (string, error) {
	path := argStr(args, "path")
	if path == "" {
		return "", errors.New("path 不能为空")
	}
	target, err := s.resolveRepo(args, false)
	if err != nil {
		return "", err
	}

	var (
		f    File
		ok   bool
		repo string
	)
	if target != nil {
		f, ok = s.index.File(target.Name, path)
		repo = target.Name
	} else {
		// 未指定仓库时在全部仓库里找；命中多个则要求澄清，不擅自挑一个。
		var found []string
		for _, r := range s.store.Repos() {
			if cand, hit := s.index.File(r.Name, path); hit {
				found = append(found, r.Name)
				f, ok, repo = cand, true, r.Name
			}
		}
		if len(found) > 1 {
			return "", fmt.Errorf("路径 %q 在多个仓库中存在（%s），请指定 repo", path, strings.Join(found, " / "))
		}
	}
	if !ok {
		return "", fmt.Errorf("未找到文件 %q。请先用 search_code 或 find_symbol 获取准确路径", path)
	}

	total := len(f.Lines)
	start := argInt(args, "start", 1)
	if start < 1 {
		start = 1
	}
	if start > total {
		return "", fmt.Errorf("%s 只有 %d 行，start=%d 越界", f.Path, total, start)
	}
	end := argInt(args, "end", 0)
	if end <= 0 {
		end = start + 119
	}
	if end > total {
		end = total
	}
	if end-start+1 > readFileMaxLines {
		end = start + readFileMaxLines - 1
	}

	var blame map[int]BlameLine
	if argBool(args, "blame") {
		if bl, err := s.store.Blame(ctx, repo, f.Path, start, end); err == nil {
			blame = make(map[int]BlameLine, len(bl))
			for _, b := range bl {
				blame[b.Line] = b
			}
		}
	}

	w := newBudget(s.cfg.MaxResponseBytes)
	title := fmt.Sprintf("%s/%s  第 %d-%d 行（共 %d 行）", repo, f.Path, start, end, total)
	if f.Lang != "" {
		title += "  [" + f.Lang + "]"
	}
	w.line(title)
	if u := s.link(repo, f.Path, start, end); u != "" {
		w.line(u)
	}
	w.line("")

	for i := start; i <= end; i++ {
		text := truncate(f.Lines[i-1], 300)
		var l string
		if b, hit := blame[i]; hit {
			l = fmt.Sprintf("%6d| %-8s %-12s| %s", i, shortSHA(b.SHA), truncate(b.Author, 12), text)
		} else {
			l = fmt.Sprintf("%6d| %s", i, text)
		}
		if !w.line(l) {
			break
		}
	}
	if end < total {
		w.line(fmt.Sprintf("…后续还有 %d 行，可用 start=%d 继续读取。", total-end, end+1))
	}
	return w.String(), nil
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// ── find_symbol ────────────────────────────────────────────

func (s *Server) toolFindSymbol(_ context.Context, args map[string]any) (string, error) {
	name := argStr(args, "name")
	if name == "" {
		return "", errors.New("name 不能为空")
	}
	target, err := s.resolveRepo(args, false)
	if err != nil {
		return "", err
	}
	repoName := ""
	if target != nil {
		repoName = target.Name
	}

	syms := s.index.FindSymbol(name, strings.ToLower(argStr(args, "kind")), repoName, argInt(args, "k", 20))
	w := newBudget(s.cfg.MaxResponseBytes)
	if len(syms) == 0 {
		w.line(fmt.Sprintf("未找到名为 %q 的定义。", name))
		w.line("建议：确认拼写；去掉 kind 过滤；或改用 search_code 做全文检索。")
		if msg := s.notReady(s.store.Repos()); msg != "" {
			w.line(msg)
		}
		return w.String(), nil
	}

	w.line(fmt.Sprintf("找到 %d 个与 %q 匹配的定义", len(syms), name))
	for i, sym := range syms {
		w.line("")
		if !w.line(fmt.Sprintf("[%d] %s %s  —  %s/%s:%d", i+1, sym.Kind, sym.Name, sym.Repo, sym.Path, sym.Line)) {
			break
		}
		if u := s.link(sym.Repo, sym.Path, sym.Line, 0); u != "" {
			w.line("    " + u)
		}
		if sym.Signature != "" {
			w.line("    " + truncate(sym.Signature, 200))
		}
		for _, d := range strings.Split(sym.Doc, "\n") {
			if strings.TrimSpace(d) == "" {
				continue
			}
			if !w.line("    // " + truncate(d, 160)) {
				break
			}
		}
	}
	return w.String(), nil
}

// ── git_history ────────────────────────────────────────────

func (s *Server) toolGitHistory(ctx context.Context, args map[string]any) (string, error) {
	target, err := s.resolveRepo(args, true)
	if err != nil {
		return "", err
	}
	path := argStr(args, "path")
	query := argStr(args, "query")
	limit := argInt(args, "limit", 10)

	commits, err := s.store.Log(ctx, target.Name, path, query, limit)
	if err != nil {
		return "", fmt.Errorf("查询 %s 的提交历史失败：%w", target.Name, err)
	}

	w := newBudget(s.cfg.MaxResponseBytes)
	if len(commits) == 0 {
		scope := target.Name
		if path != "" {
			scope += " 的 " + path
		}
		if query != "" {
			scope += "（关键词 " + query + "）"
		}
		w.line("未找到 " + scope + " 的提交记录。")
		return w.String(), nil
	}

	title := fmt.Sprintf("%s 的提交历史，共 %d 条", target.Name, len(commits))
	if path != "" {
		title += "，限定 " + path
	}
	if query != "" {
		title += "，关键词 " + query
	}
	w.line(title)

	for _, c := range commits {
		w.line("")
		if !w.line(fmt.Sprintf("%s  %s  %s", shortSHA(c.SHA), c.Date, truncate(c.Author, 20))) {
			break
		}
		w.line("  " + truncate(c.Subject, 200))
		for _, l := range strings.Split(c.Body, "\n") {
			t := strings.TrimSpace(l)
			if t == "" {
				continue
			}
			if !w.line("    " + truncate(t, 160)) {
				break
			}
		}
		if len(c.Files) > 0 {
			w.line("  改动：" + truncate(strings.Join(c.Files, ", "), 300))
		}
		if target.WebBase != "" {
			w.line("  " + target.WebBase + "/commit/" + c.SHA)
		}
	}
	return w.String(), nil
}
