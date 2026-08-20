// github.go：GitHub Issues 的 REST 客户端，实现 IssueTracker。
//
// 仅用标准库：本服务的零第三方依赖约束在这里同样成立。SDK 换来的只是类型糖，
// 代价是几十个传递依赖，不值。
//
// 权力边界是刻意设计的：只实现「读 issue / 建 issue / 评论 / 改状态与标签」，
// 不实现任何删除端点，也不碰仓库内容。即使模型被诱导，本服务能造成的最坏后果
// 也只是多一条 issue 或一条评论，可被人工撤销。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ghDefaultAPIBase = "https://api.github.com"
	ghAPIVersion     = "2022-11-28"
	ghMaxRespBytes   = 4 << 20
	ghLabelTTL       = 10 * time.Minute
	// ghMaxComments 是单个 issue 最多回传的评论数（取最新的）。
	// 长讨论串全量进小模型上下文没有意义，且输出还要过字节预算。
	ghMaxComments = 30
)

// errGHNotFound 用于让调用方区分「资源本来就不存在」与真正的失败，
// 例如移除一个本来就没打上的标签不应视为错误。
var errGHNotFound = errors.New("资源不存在")

var _ IssueTracker = (*GitHub)(nil)

// GitHub 是 IssueTracker 的 GitHub REST 实现。并发安全。
type GitHub struct {
	http *http.Client
	base string

	mu     sync.Mutex
	labels map[string]ghLabelCache
}

type ghLabelCache struct {
	names []string
	at    time.Time
}

// NewGitHub 构造客户端。base 为空时用官方 API；GitHub Enterprise 传 https://<host>/api/v3。
func NewGitHub(base string, timeout time.Duration) *GitHub {
	if strings.TrimSpace(base) == "" {
		base = ghDefaultAPIBase
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &GitHub{
		http:   &http.Client{Timeout: timeout},
		base:   strings.TrimRight(base, "/"),
		labels: make(map[string]ghLabelCache),
	}
}

// ── 传输 ────────────────────────────────────────────────────

type ghIssueJSON struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	State       string    `json:"state"`
	StateReason *string   `json:"state_reason"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Comments    int       `json:"comments"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	// PullRequest 非空表示这条其实是 PR：/issues 端点会把 PR 混在结果里。
	PullRequest *struct {
		HTMLURL string `json:"html_url"`
	} `json:"pull_request"`
}

type ghCommentJSON struct {
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

type ghReleaseJSON struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
}

func (j ghIssueJSON) toIssue() Issue {
	out := Issue{
		Number:   j.Number,
		Title:    strings.TrimSpace(j.Title),
		State:    j.State,
		Author:   j.User.Login,
		Comments: j.Comments,
		URL:      j.HTMLURL,
		Body:     strings.ReplaceAll(j.Body, "\r\n", "\n"),
	}
	if j.StateReason != nil {
		out.Reason = *j.StateReason
	}
	if !j.CreatedAt.IsZero() {
		out.CreatedAt = j.CreatedAt.UTC().Format("2006-01-02")
	}
	if !j.UpdatedAt.IsZero() {
		out.UpdatedAt = j.UpdatedAt.UTC().Format("2006-01-02")
	}
	for _, l := range j.Labels {
		if l.Name != "" {
			out.Labels = append(out.Labels, l.Name)
		}
	}
	return out
}

func (g *GitHub) do(ctx context.Context, r *Repo, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("编码请求体：%w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return fmt.Errorf("构造请求：%w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", ghAPIVersion)
	req.Header.Set("User-Agent", serverTitle+"/"+serverVersion)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.GHToken != "" {
		req.Header.Set("Authorization", "Bearer "+r.GHToken)
	}

	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("访问 GitHub 失败：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, ghMaxRespBytes))
	if err != nil {
		return fmt.Errorf("读取 GitHub 响应：%w", err)
	}
	if resp.StatusCode >= 400 {
		return ghError(resp, raw, r)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析 GitHub 响应：%w", err)
	}
	return nil
}

// ghError 把 HTTP 错误翻译成可操作的中文说明。错误文本会直接进模型上下文，
// 因此必须指出「该改配置」还是「该换参数」，而不是只回一个状态码。
func ghError(resp *http.Response, raw []byte, r *Repo) error {
	var payload struct {
		Message string `json:"message"`
		Errors  []struct {
			Field   string `json:"field"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(raw, &payload)
	msg := strings.TrimSpace(payload.Message)
	for _, e := range payload.Errors {
		part := strings.TrimSpace(e.Message)
		if part == "" {
			part = strings.TrimSpace(e.Field + " " + e.Code)
		}
		if part != "" {
			msg += "；" + part
		}
	}
	if msg == "" {
		msg = truncate(strings.TrimSpace(string(raw)), 200)
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("GitHub 拒绝令牌（401）：githubToken 无效或已过期。%s", msg)
	case http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			reset := "稍后"
			if v, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				reset = time.Unix(v, 0).UTC().Format("15:04 UTC")
			}
			return fmt.Errorf("GitHub 接口限流（403），%s 后恢复。%s", reset, msg)
		}
		return fmt.Errorf("GitHub 拒绝本次操作（403）：令牌对 %s 缺少所需权限（写 issue 需要 issues:write）。%s", r.Slug, msg)
	case http.StatusNotFound:
		return fmt.Errorf("%w（404）：目标在 %s 中找不到——issue 编号不存在，或仓库已改名、令牌无权访问。%s", errGHNotFound, r.Slug, msg)
	case http.StatusGone:
		return fmt.Errorf("该资源已被删除，或 %s 关闭了 issue 功能（410）。%s", r.Slug, msg)
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("GitHub 拒绝了参数（422）：%s", msg)
	default:
		return fmt.Errorf("GitHub 返回 %d：%s", resp.StatusCode, msg)
	}
}

// ── 读 ──────────────────────────────────────────────────────

func ghNormState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "closed":
		return "closed"
	case "all", "any", "":
		if s == "" {
			return "open"
		}
		return "all"
	default:
		return "open"
	}
}

// List 检索 issue。text 非空时优先走搜索接口（能覆盖历史 issue），
// 失败或零命中时退回「最近更新列表 + 本地打分」——搜索接口对中文分词很差，
// 中文标题的查重实际上靠的是后者。
func (g *GitHub) List(ctx context.Context, r *Repo, q IssueQuery) ([]Issue, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	state := ghNormState(q.State)
	text := strings.TrimSpace(q.Text)

	if text == "" {
		all, err := g.listPage(ctx, r, state, q.Labels, min(100, limit*2+10))
		if err != nil {
			return nil, err
		}
		if len(all) > limit {
			all = all[:limit]
		}
		return all, nil
	}

	hits, serr := g.search(ctx, r, text, state, q.Labels, limit)
	if serr == nil && len(hits) > 0 {
		return hits, nil
	}
	all, lerr := g.listPage(ctx, r, state, q.Labels, 100)
	if lerr != nil {
		if serr != nil {
			return nil, serr
		}
		return nil, lerr
	}
	return ghRankByText(all, text, limit), nil
}

func (g *GitHub) listPage(ctx context.Context, r *Repo, state string, labels []string, perPage int) ([]Issue, error) {
	if perPage < 1 {
		perPage = 30
	}
	v := url.Values{}
	v.Set("state", state)
	v.Set("sort", "updated")
	v.Set("direction", "desc")
	v.Set("per_page", strconv.Itoa(min(perPage, 100)))
	if len(labels) > 0 {
		v.Set("labels", strings.Join(labels, ","))
	}
	var raw []ghIssueJSON
	if err := g.do(ctx, r, http.MethodGet, "/repos/"+r.Slug+"/issues?"+v.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(raw))
	for _, j := range raw {
		if j.PullRequest != nil {
			continue
		}
		out = append(out, j.toIssue())
	}
	return out, nil
}

func (g *GitHub) search(ctx context.Context, r *Repo, text, state string, labels []string, limit int) ([]Issue, error) {
	q := "repo:" + r.Slug + " is:issue " + text
	if state == "open" || state == "closed" {
		q += " state:" + state
	}
	for _, l := range labels {
		q += " label:" + strconv.Quote(l)
	}
	v := url.Values{}
	v.Set("q", q)
	v.Set("per_page", strconv.Itoa(min(limit, 50)))
	v.Set("sort", "updated")
	v.Set("order", "desc")

	var resp struct {
		Items []ghIssueJSON `json:"items"`
	}
	if err := g.do(ctx, r, http.MethodGet, "/search/issues?"+v.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(resp.Items))
	for _, j := range resp.Items {
		if j.PullRequest != nil {
			continue
		}
		out = append(out, j.toIssue())
	}
	return out, nil
}

// PR 是 GitHub Pull Request 的查询结果（只读）。
type PR struct {
	Number    int
	Title     string
	State     string // open / closed（已合并的 PR 状态为 closed，看 Merged）
	Merged    bool
	Draft     bool // 草稿 PR：未就绪、不可合并，state 仍为 open
	Author    string
	HeadRef   string
	BaseRef   string
	Comments  int
	CreatedAt string
	UpdatedAt string
	URL       string
	Body      string
}

type ghPRJSON struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	Comments  int       `json:"comments"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Merged    bool      `json:"merged"`
	Draft     bool      `json:"draft"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (j ghPRJSON) toPR() PR {
	p := PR{
		Number:   j.Number,
		Title:    strings.TrimSpace(j.Title),
		State:    j.State,
		Merged:   j.Merged,
		Draft:    j.Draft,
		Author:   j.User.Login,
		HeadRef:  j.Head.Ref,
		BaseRef:  j.Base.Ref,
		Comments: j.Comments,
		URL:      j.HTMLURL,
		Body:     strings.ReplaceAll(j.Body, "\r\n", "\n"),
	}
	if !j.CreatedAt.IsZero() {
		p.CreatedAt = j.CreatedAt.UTC().Format("2006-01-02")
	}
	if !j.UpdatedAt.IsZero() {
		p.UpdatedAt = j.UpdatedAt.UTC().Format("2006-01-02")
	}
	return p
}

// ListPRs 按状态列出 PR，按更新时间倒序。只读查询。
func (g *GitHub) ListPRs(ctx context.Context, r *Repo, state string, limit int) ([]PR, error) {
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 30)
	v := url.Values{}
	v.Set("state", ghNormState(state))
	v.Set("sort", "updated")
	v.Set("direction", "desc")
	v.Set("per_page", strconv.Itoa(limit))
	var raw []ghPRJSON
	if err := g.do(ctx, r, http.MethodGet, "/repos/"+r.Slug+"/pulls?"+v.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]PR, 0, len(raw))
	for _, j := range raw {
		out = append(out, j.toPR())
	}
	return out, nil
}

// GetPR 读取单个 PR 的完整描述与状态（含合并信息）。
func (g *GitHub) GetPR(ctx context.Context, r *Repo, number int) (PR, error) {
	var j ghPRJSON
	if err := g.do(ctx, r, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", r.Slug, number), nil, &j); err != nil {
		return PR{}, err
	}
	return j.toPR(), nil
}

// PRComments 返回 PR 的讨论评论（issue comments 端点），最新优先。
// 与 issue 评论同构，复用 ghCommentJSON。
func (g *GitHub) PRComments(ctx context.Context, r *Repo, number, limit int) ([]IssueComment, error) {
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, ghMaxComments)
	v := url.Values{}
	v.Set("per_page", strconv.Itoa(limit))
	var raw []ghCommentJSON
	if err := g.do(ctx, r, http.MethodGet, fmt.Sprintf("/repos/%s/issues/%d/comments?%s", r.Slug, number, v.Encode()), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]IssueComment, 0, len(raw))
	for _, c := range raw {
		date := ""
		if !c.CreatedAt.IsZero() {
			date = c.CreatedAt.UTC().Format("2006-01-02")
		}
		out = append(out, IssueComment{
			Author: c.User.Login,
			Date:   date,
			Body:   strings.ReplaceAll(c.Body, "\r\n", "\n"),
		})
	}
	return out, nil
}

// Releases 返回仓库最近 n 个发布。草稿不算发布（draft=true 的条目跳过），
// 否则「最新版本」会答出还没公开的草稿；pre-release 保留，它是真实发布的版本。
func (g *GitHub) Releases(ctx context.Context, r *Repo, n int) ([]Release, error) {
	if n <= 0 {
		n = 5
	}
	n = min(n, 20)
	v := url.Values{}
	v.Set("per_page", strconv.Itoa(n))

	var raw []ghReleaseJSON
	if err := g.do(ctx, r, http.MethodGet, "/repos/"+r.Slug+"/releases?"+v.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(raw))
	for _, j := range raw {
		if j.Draft {
			continue
		}
		out = append(out, Release{
			Tag:         j.TagName,
			Name:        j.Name,
			Body:        strings.ReplaceAll(j.Body, "\r\n", "\n"),
			PublishedAt: j.PublishedAt.UTC().Format("2006-01-02"),
			URL:         j.HTMLURL,
		})
	}
	return out, nil
}

// Get 返回 issue 正文与最近若干条评论。评论取不到不影响正文返回——
// 正文是主要证据，为了评论把整次调用判失败不划算。
func (g *GitHub) Get(ctx context.Context, r *Repo, number int) (Issue, []IssueComment, error) {
	base := fmt.Sprintf("/repos/%s/issues/%d", r.Slug, number)
	var j ghIssueJSON
	if err := g.do(ctx, r, http.MethodGet, base, nil, &j); err != nil {
		return Issue{}, nil, err
	}
	if j.PullRequest != nil {
		return Issue{}, nil, fmt.Errorf("#%d 是 Pull Request 而不是 issue", number)
	}
	iss := j.toIssue()
	if j.Comments == 0 {
		return iss, nil, nil
	}

	v := url.Values{}
	v.Set("per_page", "100")
	var raw []ghCommentJSON
	if err := g.do(ctx, r, http.MethodGet, base+"/comments?"+v.Encode(), nil, &raw); err != nil {
		return iss, nil, nil
	}
	if len(raw) > ghMaxComments {
		raw = raw[len(raw)-ghMaxComments:]
	}
	out := make([]IssueComment, 0, len(raw))
	for _, c := range raw {
		date := ""
		if !c.CreatedAt.IsZero() {
			date = c.CreatedAt.UTC().Format("2006-01-02")
		}
		out = append(out, IssueComment{
			Author: c.User.Login,
			Date:   date,
			Body:   strings.ReplaceAll(c.Body, "\r\n", "\n"),
		})
	}
	return iss, out, nil
}

// RepoLabels 返回仓库现有标签，带 10 分钟缓存。
// 用途是过滤模型编造的标签：GitHub 允许打标签时顺手新建标签，
// 不校验就会让机器人污染仓库的标签体系。
func (g *GitHub) RepoLabels(ctx context.Context, r *Repo) ([]string, error) {
	g.mu.Lock()
	c, ok := g.labels[r.Slug]
	g.mu.Unlock()
	if ok && time.Since(c.at) < ghLabelTTL {
		return c.names, nil
	}

	v := url.Values{}
	v.Set("per_page", "100")
	var raw []struct {
		Name string `json:"name"`
	}
	if err := g.do(ctx, r, http.MethodGet, "/repos/"+r.Slug+"/labels?"+v.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(raw))
	for _, l := range raw {
		if l.Name != "" {
			names = append(names, l.Name)
		}
	}
	g.mu.Lock()
	g.labels[r.Slug] = ghLabelCache{names: names, at: time.Now()}
	g.mu.Unlock()
	return names, nil
}

// ── 写 ──────────────────────────────────────────────────────

func (g *GitHub) Create(ctx context.Context, r *Repo, d IssueDraft) (Issue, error) {
	payload := map[string]any{"title": d.Title, "body": d.Body}
	if len(d.Labels) > 0 {
		payload["labels"] = d.Labels
	}
	var j ghIssueJSON
	if err := g.do(ctx, r, http.MethodPost, "/repos/"+r.Slug+"/issues", payload, &j); err != nil {
		return Issue{}, err
	}
	return j.toIssue(), nil
}

func (g *GitHub) Comment(ctx context.Context, r *Repo, number int, body string) error {
	p := fmt.Sprintf("/repos/%s/issues/%d/comments", r.Slug, number)
	return g.do(ctx, r, http.MethodPost, p, map[string]any{"body": body}, nil)
}

// Edit 按「先删标签、再加标签、最后改状态」的顺序执行。
// 状态放最后，保证 issue 被关闭时标签已经是最终形态，通知里的信息才完整。
func (g *GitHub) Edit(ctx context.Context, r *Repo, number int, e IssueEdit) (Issue, error) {
	base := fmt.Sprintf("/repos/%s/issues/%d", r.Slug, number)

	for _, name := range e.RemoveLabels {
		err := g.do(ctx, r, http.MethodDelete, base+"/labels/"+url.PathEscape(name), nil, nil)
		if err != nil && !errors.Is(err, errGHNotFound) {
			return Issue{}, err
		}
	}
	if len(e.AddLabels) > 0 {
		if err := g.do(ctx, r, http.MethodPost, base+"/labels", map[string]any{"labels": e.AddLabels}, nil); err != nil {
			return Issue{}, err
		}
	}

	var j ghIssueJSON
	if e.State == "" && e.Title == "" && e.Body == "" {
		if err := g.do(ctx, r, http.MethodGet, base, nil, &j); err != nil {
			return Issue{}, err
		}
		return j.toIssue(), nil
	}
	payload := make(map[string]any, 4)
	if e.Title != "" {
		payload["title"] = e.Title
	}
	if e.Body != "" {
		payload["body"] = e.Body
	}
	if e.State != "" {
		payload["state"] = e.State
	}
	if e.State == "closed" && e.StateReason != "" {
		payload["state_reason"] = e.StateReason
	}
	if e.State == "open" {
		payload["state_reason"] = "reopened"
	}
	if err := g.do(ctx, r, http.MethodPatch, base, payload, &j); err != nil {
		return Issue{}, err
	}
	return j.toIssue(), nil
}

// MediaReferenced 判断查询词是否命中该仓库的 issue 搜索（search API 对正文全文索引）。
// 任何错误都返回 (false, err)，由调用方 fail-safe 跳过删除。
func (g *GitHub) MediaReferenced(ctx context.Context, repoSlug, token, query string) (bool, error) {
	repo := &Repo{Slug: repoSlug, GHToken: token}
	hits, err := g.search(ctx, repo, query, "all", nil, 1)
	if err != nil {
		return false, err
	}
	return len(hits) > 0, nil
}

// MediaReferencedGlobal 用全局令牌做不带 repo 限定的 issue 全文检索：
// 引用媒体 hex 的 issue 可能在任意 token 可达仓库（含未配置仓库），只查
// 配置仓库会漏判导致误删。任何错误都返回 (false, err)，由调用方
// fail-safe 跳过删除。
func (g *GitHub) MediaReferencedGlobal(ctx context.Context, token, query string) (bool, error) {
	repo := &Repo{GHToken: token}
	v := url.Values{}
	v.Set("q", "is:issue "+query)
	v.Set("per_page", "1")
	v.Set("sort", "updated")
	v.Set("order", "desc")

	var resp struct {
		Items []ghIssueJSON `json:"items"`
	}
	if err := g.do(ctx, repo, http.MethodGet, "/search/issues?"+v.Encode(), nil, &resp); err != nil {
		return false, err
	}
	for _, j := range resp.Items {
		if j.PullRequest != nil {
			continue
		}
		return true, nil
	}
	return false, nil
}

// ── 文本匹配 ────────────────────────────────────────────────
//
// 供两处使用：搜索接口不可用时的本地排序，以及创建前的查重。
// 复用索引层的 ixTokenize（能拆 camelCase / snake_case），
// 另加 CJK 二元组——中文标题占 IM 场景的绝大多数，只切 ASCII 等于没切。

var ghStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "when": true, "this": true,
	"that": true, "issue": true, "bug": true, "问题": true, "报错": true, "无法": true,
}

// ghTextTokens 把一段文本切成可比较的 token 集合。
func ghTextTokens(s string) map[string]bool {
	out := make(map[string]bool, 16)
	for _, t := range ixTokenize(s) {
		if !ghStopWords[t] {
			out[t] = true
		}
	}
	// CJK 二元组：ixTokenize 只认 [A-Za-z0-9_]，中文全被丢掉。
	runes := []rune(s)
	var run []rune
	flush := func() {
		for i := 0; i+1 < len(run); i++ {
			bg := string(run[i : i+2])
			if !ghStopWords[bg] {
				out[bg] = true
			}
		}
		run = run[:0]
	}
	for _, r := range runes {
		if ghIsCJK(r) {
			run = append(run, r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func ghIsCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // 汉字
		(r >= 0x3040 && r <= 0x30FF) || // 假名
		(r >= 0xAC00 && r <= 0xD7AF) // 谚文
}

// ghSimilarity 用重叠系数（交集 / 较小集合）而非 Jaccard：
// 查重要比的是「短标题是否被长标题覆盖」，Jaccard 会因长度差异把真重复压到阈值以下。
func ghSimilarity(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	hit := 0
	for t := range small {
		if large[t] {
			hit++
		}
	}
	return float64(hit) / float64(len(small))
}

// ghRankByText 按与 text 的重合度对候选排序，零重合的直接丢弃。
func ghRankByText(items []Issue, text string, limit int) []Issue {
	qt := ghTextTokens(text)
	type scored struct {
		iss   Issue
		score float64
	}
	ranked := make([]scored, 0, len(items))
	for _, it := range items {
		s := ghSimilarity(qt, ghTextTokens(it.Title))
		if b := ghSimilarity(qt, ghTextTokens(truncate(it.Body, 600))); b > s {
			// 正文命中比标题命中弱：只作补充，不足以单独把结果顶上来。
			s = s*0.5 + b*0.5
		}
		if s <= 0 {
			continue
		}
		ranked = append(ranked, scored{it, s})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]Issue, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, r.iss)
	}
	return out
}
