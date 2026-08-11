// 配置加载：JSON 配置文件 + 环境变量覆盖。零依赖，不引入 YAML 解析。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config 是服务的全部配置。
type Config struct {
	// Listen 监听地址，默认 :8790。
	Listen string `json:"listen"`
	// Token 是 Bearer 鉴权令牌。留空则不鉴权（仅建议在 127.0.0.1 上）。
	Token string `json:"token"`
	// DataDir 存放各仓库的本地 clone，默认 ./data。
	DataDir string `json:"dataDir"`
	// SyncInterval 是后台拉取远端并重建索引的周期，Go duration；"0" 关闭定时同步。
	SyncInterval string `json:"syncInterval"`
	// MaxResponseBytes 是单次工具返回的字节预算，默认 12000。
	// LangBot 不会截断 tool 返回，必须由本服务自己收口，否则会撑爆 IM 侧小模型上下文。
	MaxResponseBytes int `json:"maxResponseBytes"`
	// GitTimeout 是单条 git 命令的超时，Go duration，默认 3m。
	GitTimeout string `json:"gitTimeout"`

	// GitHubAPIBase 是 GitHub REST API 根地址，默认 https://api.github.com；
	// GitHub Enterprise 填 https://<host>/api/v3。
	GitHubAPIBase string `json:"githubApiBase"`
	// GitHubToken 是 issue 工具使用的默认令牌（PAT，写操作需要 issues:write）；
	// 可被 repos[].issues.token 覆盖，环境变量 REPOMCP_GITHUB_TOKEN 最优先。
	GitHubToken string `json:"githubToken"`
	// GitHubTimeout 是单次 GitHub API 调用超时，Go duration，默认 20s。
	GitHubTimeout string `json:"githubTimeout"`
	// MaxIssueCreatesPerHour 限制单仓每小时可创建的 issue 数，默认 5，0 表示不限。
	// 这是防御性的：模型在对话里反复"帮你提个 issue"是真实风险，提示词约束不住。
	MaxIssueCreatesPerHour *int `json:"maxIssueCreatesPerHour"`

	// AdminReporters 是管理员报告人标识列表（昵称、QQ 号，或 昵称(QQ号) 完整格式）。
	// 管理员对任意仓库（token 可访问的，含未配置仓库）拥有 issue 写入与修改权限；
	// 非管理员只能对配置白名单仓库写入。列表不固定仓库，管理员要查/要改哪个仓库由对话指定。
	AdminReporters []string `json:"adminReporters"`

	Repos []RepoConfig `json:"repos"`

	// 解析后的派生值。
	syncInterval time.Duration
	gitTimeout   time.Duration
	ghTimeout    time.Duration
	issueLimit   int // 每仓每小时创建上限，0 = 不限
}

// RepoConfig 是配置文件中的一个仓库条目。
type RepoConfig struct {
	// Name 是短名，LLM 用它作为工具参数。建议全小写无空格。
	Name string `json:"name"`
	// URL 是远端地址；私有仓请在 URL 内嵌 token 或预先配置好凭据助手。
	URL string `json:"url"`
	// Ref 是跟踪分支，默认 main。
	Ref string `json:"ref"`
	// WebBase 是 permalink 前缀（如 https://github.com/owner/repo）。
	// 留空时会尝试从 URL 推导；仍推导不出则结果不含链接。
	WebBase string `json:"webBase"`
	// Dir 可覆盖本地路径；留空则为 <DataDir>/<Name>。
	// 指向一个已存在的本地仓库时，同步阶段仍会 fetch+reset，请勿指向你的开发工作树。
	Dir string `json:"dir"`
	// Desc 是一句话说明，会出现在 repo_overview 里帮助 LLM 选仓。
	Desc string `json:"desc"`

	Include []string `json:"include"`
	Exclude []string `json:"exclude"`

	// Code 是否作为源码仓库克隆并索引；false 表示反馈类仓库（无源码，仅 issue 能力）。
	// 指针语义：缺省 nil = true，显式 "code": false 才关闭，避免遗漏配置误伤现有仓库。
	Code *bool `json:"code"`

	// Issues 开启该仓的 issue 能力；省略则该仓不出现在任何 issue 工具里。
	Issues *RepoIssuesConfig `json:"issues"`
}

// RepoIssuesConfig 控制单个仓库的 issue 能力。给出空对象 {} 即为「只读检索」。
type RepoIssuesConfig struct {
	// Slug 是 owner/repo；留空则从 webBase / url 推导（仅 GitHub 风格地址可推导）。
	Slug string `json:"slug"`
	// Write 允许创建 issue、评论与改状态；默认 false，即只读。
	Write bool `json:"write"`
	// Token 覆盖全局 githubToken。
	Token string `json:"token"`
	// Labels 是允许模型使用的标签白名单；留空表示以仓库现有标签为准。
	Labels []string `json:"labels"`
}

var reRepoName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// LoadConfig 读取配置文件并应用环境变量覆盖。
// 环境变量：REPOMCP_CONFIG / REPOMCP_LISTEN / REPOMCP_TOKEN / REPOMCP_DATA。
func LoadConfig(path string) (*Config, error) {
	if v := os.Getenv("REPOMCP_CONFIG"); v != "" {
		path = v
	}
	cfg := &Config{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}

	if v := os.Getenv("REPOMCP_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := os.Getenv("REPOMCP_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("REPOMCP_DATA"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("REPOMCP_GITHUB_TOKEN"); v != "" {
		cfg.GitHubToken = v
	}

	if cfg.Listen == "" {
		cfg.Listen = ":8790"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "./data"
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = 12000
	}
	if cfg.MaxResponseBytes < 2000 {
		return nil, errors.New("maxResponseBytes 不应小于 2000，否则检索结果无法承载证据")
	}

	cfg.syncInterval, err = parseDur(cfg.SyncInterval, 15*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("syncInterval: %w", err)
	}
	cfg.gitTimeout, err = parseDur(cfg.GitTimeout, 3*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("gitTimeout: %w", err)
	}
	if cfg.gitTimeout <= 0 {
		cfg.gitTimeout = 3 * time.Minute
	}
	cfg.ghTimeout, err = parseDur(cfg.GitHubTimeout, 20*time.Second)
	if err != nil {
		return nil, fmt.Errorf("githubTimeout: %w", err)
	}
	if cfg.ghTimeout <= 0 {
		cfg.ghTimeout = 20 * time.Second
	}
	cfg.issueLimit = 5
	if n := cfg.MaxIssueCreatesPerHour; n != nil {
		if *n < 0 {
			return nil, errors.New("maxIssueCreatesPerHour 不能为负；0 表示不限")
		}
		cfg.issueLimit = *n
	}

	if len(cfg.Repos) == 0 {
		return nil, errors.New("repos 为空：至少配置一个仓库")
	}
	return cfg, nil
}

func parseDur(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	if s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	return d, nil
}

// BuildRepos 把配置条目解析为运行时 Repo（Dir 绝对化、WebBase 推导、重名校验）。
func (c *Config) BuildRepos() ([]*Repo, error) {
	dataDir, err := filepath.Abs(c.DataDir)
	if err != nil {
		return nil, fmt.Errorf("解析 dataDir: %w", err)
	}
	seen := make(map[string]bool, len(c.Repos))
	out := make([]*Repo, 0, len(c.Repos))
	for i, rc := range c.Repos {
		name := strings.ToLower(strings.TrimSpace(rc.Name))
		if !reRepoName.MatchString(name) {
			return nil, fmt.Errorf("repos[%d].name %q 非法：要求 ^[a-z0-9][a-z0-9._-]{0,63}$", i, rc.Name)
		}
		if seen[name] {
			return nil, fmt.Errorf("repos[%d].name %q 重复", i, name)
		}
		seen[name] = true
		if strings.TrimSpace(rc.URL) == "" {
			return nil, fmt.Errorf("repos[%d] (%s).url 为空", i, name)
		}

		dir := rc.Dir
		if dir == "" {
			dir = filepath.Join(dataDir, name)
		}
		dir, err = filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("repos[%d] (%s).dir: %w", i, name, err)
		}

		ref := rc.Ref
		if ref == "" {
			ref = "main"
		}
		web := strings.TrimRight(rc.WebBase, "/")
		if web == "" {
			web = deriveWebBase(rc.URL)
		}

		r := &Repo{
			Name:    name,
			URL:     rc.URL,
			Ref:     ref,
			Dir:     dir,
			WebBase: web,
			Include: rc.Include,
			Exclude: rc.Exclude,
			HasCode: rc.Code == nil || *rc.Code,
		}
		if err := c.applyIssues(r, rc, i); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

var reRepoSlug = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// applyIssues 解析单仓的 issue 配置：推导 slug、挑令牌、校验写能力的前置条件。
// 写能力必须有令牌——匿名调用 GitHub 根本写不了，与其运行期报 401，不如启动就拒绝。
func (c *Config) applyIssues(r *Repo, rc RepoConfig, i int) error {
	if rc.Issues == nil {
		return nil
	}
	slug := strings.Trim(strings.TrimSpace(rc.Issues.Slug), "/")
	if slug == "" {
		slug = deriveSlug(r.WebBase, rc.URL)
	}
	if slug == "" {
		return fmt.Errorf("repos[%d] (%s).issues：无法从 url/webBase 推导 owner/repo，请显式填写 issues.slug", i, r.Name)
	}
	if !reRepoSlug.MatchString(slug) {
		return fmt.Errorf("repos[%d] (%s).issues.slug %q 非法：要求 owner/repo 形式", i, r.Name, slug)
	}
	token := strings.TrimSpace(rc.Issues.Token)
	if token == "" {
		token = strings.TrimSpace(c.GitHubToken)
	}
	if rc.Issues.Write && token == "" {
		return fmt.Errorf("repos[%d] (%s).issues.write=true 但无令牌：请配置 githubToken 或 repos[].issues.token", i, r.Name)
	}
	r.Slug = slug
	r.GHToken = token
	r.IssueRead = true
	r.IssueWrite = rc.Issues.Write
	r.IssueLabels = rc.Issues.Labels
	return nil
}

// deriveSlug 从网页前缀（或据 clone 地址推导出的网页前缀）取出 owner/repo。
func deriveSlug(webBase, url string) string {
	base := strings.TrimRight(webBase, "/")
	if base == "" {
		base = deriveWebBase(url)
	}
	if base == "" {
		return ""
	}
	if i := strings.Index(base, "://"); i >= 0 {
		base = base[i+3:]
	}
	// 首段是主机名，其后两段即 owner/repo。
	parts := strings.Split(strings.Trim(base, "/"), "/")
	if len(parts) < 3 {
		return ""
	}
	owner, name := parts[1], strings.TrimSuffix(parts[2], ".git")
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}

// deriveWebBase 从 clone URL 推导网页前缀，支持 https 与 scp 风格 ssh 地址。
// 推导不出（本地路径、自建非常规地址）时返回空串，调用方据此省略 permalink。
func deriveWebBase(url string) string {
	u := strings.TrimSpace(url)
	u = strings.TrimSuffix(u, ".git")
	switch {
	case strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"):
		// 去掉可能内嵌的凭据，避免 token 被写进给 LLM 的链接里。
		if i := strings.Index(u, "://"); i >= 0 {
			rest := u[i+3:]
			if at := strings.Index(rest, "@"); at >= 0 && at < strings.Index(rest+"/", "/") {
				u = u[:i+3] + rest[at+1:]
			}
		}
		return strings.TrimRight(u, "/")
	case strings.HasPrefix(u, "git@"):
		rest := strings.TrimPrefix(u, "git@")
		host, path, ok := strings.Cut(rest, ":")
		if !ok || host == "" || path == "" {
			return ""
		}
		return "https://" + host + "/" + strings.TrimLeft(path, "/")
	default:
		return ""
	}
}

// desc 按名字取回配置里的一句话说明。
func (c *Config) desc(name string) string {
	for _, rc := range c.Repos {
		if strings.EqualFold(strings.TrimSpace(rc.Name), name) {
			return rc.Desc
		}
	}
	return ""
}

// itoa 供输出格式化使用，避免各处重复导入 strconv。
func itoa(n int) string { return strconv.Itoa(n) }
