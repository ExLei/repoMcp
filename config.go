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

	Repos []RepoConfig `json:"repos"`

	// 解析后的派生值。
	syncInterval time.Duration
	gitTimeout   time.Duration
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

		out = append(out, &Repo{
			Name:    name,
			URL:     rc.URL,
			Ref:     ref,
			Dir:     dir,
			WebBase: web,
			Include: rc.Include,
			Exclude: rc.Exclude,
		})
	}
	return out, nil
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
