// 跨模块契约：本文件是 repoMcp 各层之间唯一的类型边界。
// 修改此处等于修改模块间协议，必须同步 index.go / repo.go / tools.go 三侧。
package main

import "context"

// Repo 是白名单中的一个仓库（运行时形态，Dir 已解析为绝对路径）。
type Repo struct {
	Name    string // 短名，作为工具参数与结果中的仓库标识
	URL     string // 远端地址
	Ref     string // 跟踪分支，如 main
	Dir     string // 本地工作树绝对路径
	WebBase string // permalink 前缀，如 https://github.com/owner/repo；空则不产出链接
	Include []string
	Exclude []string
}

// File 是索引中的一个文件快照。Lines 不含行尾换行符。
type File struct {
	Repo  string
	Path  string // 仓库内相对路径，一律 / 分隔
	Lang  string
	Lines []string
}

// Hit 是一条检索证据。Snippet 已包含上下文并带行号前缀，可直接进 LLM 上下文。
type Hit struct {
	Repo    string
	Path    string
	Line    int // 命中主行，1-based
	EndLine int
	Score   float64
	Snippet string
	Why     string // 命中原因，如 "symbol:decode" / "term:retry"
}

// Symbol 是一个定义点。
type Symbol struct {
	Repo      string
	Path      string
	Line      int
	Kind      string // func/method/type/struct/class/interface/trait/enum/const/var
	Name      string
	Signature string
	Doc       string // 紧邻定义上方的文档注释，已去掉注释前缀
}

// SearchQuery 是一次混合检索请求。Repo/Lang/PathGlob 为空表示不过滤。
type SearchQuery struct {
	Text     string
	Repo     string
	Lang     string
	PathGlob string
	K        int
}

// RepoStats 是单仓索引概况，供 repo_overview 使用。
type RepoStats struct {
	Files   int
	Lines   int
	Symbols int
	ByLang  map[string]int // 语言 -> 文件数
}

// Commit 是一条提交记录。
type Commit struct {
	SHA     string
	Author  string
	Date    string
	Subject string
	Body    string
	Files   []string
}

// BlameLine 是一行的归属信息。
type BlameLine struct {
	Line   int
	SHA    string
	Author string
	Date   string
	Text   string
}

// Indexer 是检索层对外契约，由 *Index 实现。实现必须并发安全：
// Replace 与查询方法会被后台同步协程和请求协程并发调用。
type Indexer interface {
	// Replace 原子替换某仓的全部文档与符号。
	Replace(repo string, files []File)
	Search(q SearchQuery) []Hit
	FindSymbol(name, kind, repo string, k int) []Symbol
	// Tree 返回目录结构摘要行（已按重要性裁剪到 maxEntries 条）。
	Tree(repo string, maxEntries int) []string
	File(repo, path string) (File, bool)
	Stats() map[string]RepoStats
}

// Storer 是仓库与 git 层对外契约，由 *Store 实现。
type Storer interface {
	Repos() []*Repo
	Get(name string) (*Repo, bool)
	// Sync 对所有仓执行 clone 或 fetch+reset，返回首个致命错误。
	Sync(ctx context.Context) error
	// Load 读取工作树中所有符合 Include/Exclude 且非二进制的文件。
	Load(r *Repo) ([]File, error)
	// Head 返回当前 commit sha，未就绪时返回空串。
	Head(repo string) string
	// Log 查询提交历史。path 与 grep 均可为空。
	Log(ctx context.Context, repo, path, grep string, n int) ([]Commit, error)
	Blame(ctx context.Context, repo, path string, start, end int) ([]BlameLine, error)
}
