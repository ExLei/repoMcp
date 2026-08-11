// repo.go：RepoStore —— 基于系统 git 子进程管理白名单仓库的克隆/同步/文件加载/日志/blame。
// 本模块不解析 .git 内部结构，一律通过 `git` 命令行完成，便于跟随系统 git 的行为与安全策略。
// 本文件不引入任何平台特有 syscall 字段（如 Windows 下隐藏子进程黑窗所需的 SysProcAttr），
// 以保证 CGO_ENABLED=0 下的跨平台交叉编译；开发机（Windows）运行时子进程可能短暂闪现控制台窗口，可接受。
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ---- 基础设施：子进程执行 git ----

// stRunGit 在 dir 目录下以子进程方式执行 git 命令，捕获 stdout；非零退出时返回
// 包含 stderr 摘要（截断至 500 字符）的错误。调用方必须提供带超时/取消的 context。
func stRunGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GIT_CONFIG_NOSYSTEM=1",
		"LC_ALL=C",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.String(), nil
}

// IsProbablyBinary 判断内容是否可能为二进制：前 8KB 内含 NUL 字节，或非法 UTF-8
// 字节序列比例过高（>1%），均判定为二进制。
func IsProbablyBinary(b []byte) bool {
	n := len(b)
	if n > 8192 {
		n = 8192
	}
	chunk := b[:n]
	if bytes.IndexByte(chunk, 0) >= 0 {
		return true
	}
	if n == 0 || utf8.Valid(chunk) {
		return false
	}
	invalid, total := 0, 0
	for i := 0; i < len(chunk); {
		r, size := utf8.DecodeRune(chunk[i:])
		total++
		if r == utf8.RuneError && size == 1 {
			invalid++
		}
		i += size
	}
	return total > 0 && float64(invalid)/float64(total) > 0.01
}

// stSafeRelPath 校验并规整仓库内相对路径：拒绝绝对路径与包含 ".." 段的路径，
// 统一为 "/" 分隔。用于抵御路径逃逸。
func stSafeRelPath(p string) (string, bool) {
	p = filepath.ToSlash(strings.TrimSpace(p))
	if p == "" || strings.HasPrefix(p, "/") {
		return "", false
	}
	if len(p) >= 2 && p[1] == ':' { // Windows 盘符绝对路径，如 C:/...
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return "", false
		}
	}
	return p, true
}

// ---- 硬排除规则（无论调用方 Include/Exclude 如何配置都生效）----

var stHardExcludeDirNames = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "target": true,
	"build": true, "dist": true, ".next": true, "__pycache__": true,
}

var stHardExcludeExt = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "gif": true, "webp": true, "ico": true, "svg": true,
	"pdf": true, "zip": true, "gz": true, "tar": true, "7z": true, "rar": true,
	"exe": true, "dll": true, "so": true, "dylib": true, "a": true, "o": true, "bin": true, "wasm": true,
	"mp4": true, "mp3": true, "wav": true, "ttf": true, "otf": true, "woff": true, "woff2": true,
	"lock": true, "db": true, "sqlite": true,
}

var stHardExcludeNames = map[string]bool{
	"bun.lockb": true, "pnpm-lock.yaml": true, "package-lock.json": true,
}

// stIsHardExcluded 判断仓库内相对路径（"/" 分隔）是否命中内置硬排除规则。
func stIsHardExcluded(relPath string) bool {
	segs := strings.Split(relPath, "/")
	for _, seg := range segs[:len(segs)-1] {
		if stHardExcludeDirNames[seg] {
			return true
		}
	}
	base := segs[len(segs)-1]
	if stHardExcludeNames[base] {
		return true
	}
	if dot := strings.LastIndexByte(base, '.'); dot >= 0 && dot < len(base)-1 {
		ext := strings.ToLower(base[dot+1:])
		if stHardExcludeExt[ext] {
			return true
		}
	}
	return false
}

// ---- Store ----

// stRepoStatus 记录单仓最近一次同步的状态，受 Store.mu 保护。
type stRepoStatus struct {
	head     string
	lastSync time.Time
	lastErr  error
}

// Store 实现 Storer 接口：以系统 git 命令管理构造时传入的白名单仓库集合。
type Store struct {
	repos  []*Repo
	byName map[string]*Repo

	mu     sync.RWMutex
	status map[string]*stRepoStatus
}

var _ Storer = (*Store)(nil)

// NewStore 用给定仓库列表构造 Store；repos 中的 Dir 需为已解析好的绝对路径。
func NewStore(repos []*Repo) *Store {
	s := &Store{
		repos:  repos,
		byName: make(map[string]*Repo, len(repos)),
		status: make(map[string]*stRepoStatus, len(repos)),
	}
	for _, r := range repos {
		s.byName[r.Name] = r
		s.status[r.Name] = &stRepoStatus{}
	}
	return s
}

// Repos 按构造时传入的顺序返回仓库列表副本。
func (s *Store) Repos() []*Repo {
	out := make([]*Repo, len(s.repos))
	copy(out, s.repos)
	return out
}

// Get 按短名查找仓库。
func (s *Store) Get(name string) (*Repo, bool) {
	r, ok := s.byName[name]
	return r, ok
}

// Status 返回某仓库最近一次同步后的缓存 HEAD、成功时间与最后一次错误（额外方法，非 Storer 契约的一部分）。
func (s *Store) Status(repo string) (head string, lastSync time.Time, lastErr error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.status[repo]
	if !ok {
		return "", time.Time{}, nil
	}
	return st.head, st.lastSync, st.lastErr
}

// Sync 对每个仓库执行克隆或增量拉取；单仓失败不影响其它仓库，全部结果用 errors.Join 聚合返回。
// 反馈仓库（HasCode=false）不克隆不拉取。
func (s *Store) Sync(ctx context.Context) error {
	var errs []error
	for _, r := range s.repos {
		if !r.HasCode {
			continue
		}
		if err := s.stSyncOne(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("repo %s: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}

// stIsGitRepo 用 git 自身判定目录是否为有效仓库。
// 只 stat .git 是不够的：中断的 clone 会留下 .git 残骸，
// 据此走增量路径会永远失败，且永远不会重新克隆——仓库就此卡死。
func stIsGitRepo(ctx context.Context, dir string) bool {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return false
	}
	_, err := stRunGit(ctx, dir, "rev-parse", "--git-dir")
	return err == nil
}

// stClone 全新克隆到 r.Dir。
func (s *Store) stClone(ctx context.Context, r *Repo) error {
	if err := os.MkdirAll(filepath.Dir(r.Dir), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	args := []string{"clone", "--depth", "200", "--single-branch"}
	if r.Ref != "" {
		args = append(args, "--branch", r.Ref)
	}
	args = append(args, r.URL, r.Dir)
	_, err := stRunGit(ctx, filepath.Dir(r.Dir), args...)
	return err
}

// stPull 增量拉取并把工作树强制对齐远端。
func (s *Store) stPull(ctx context.Context, r *Repo) error {
	ref := r.Ref
	if ref == "" {
		ref = "HEAD"
	}
	if _, err := stRunGit(ctx, r.Dir, "fetch", "--depth", "200", "origin", ref); err != nil {
		return err
	}
	if _, err := stRunGit(ctx, r.Dir, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return err
	}
	_, err := stRunGit(ctx, r.Dir, "clean", "-fdq")
	return err
}

// stSyncOne 同步单个仓库：无效目录重建，有效目录增量拉取。
// 关键不变式——任何一轮失败都不能让该仓库永久失去恢复能力。
func (s *Store) stSyncOne(ctx context.Context, r *Repo) error {
	var syncErr error

	switch {
	case !stIsGitRepo(ctx, r.Dir):
		// 目录存在却不是有效仓库，说明是上次中断留下的残骸；清掉重来。
		if _, err := os.Stat(r.Dir); err == nil {
			if err := os.RemoveAll(r.Dir); err != nil {
				syncErr = fmt.Errorf("清理无效仓库目录 %s: %w", r.Dir, err)
				break
			}
		}
		syncErr = s.stClone(ctx, r)

	default:
		if err := s.stPull(ctx, r); err != nil {
			// 增量失败后复检：仓库仍有效说明多半是网络或权限问题，
			// 保留本地数据只报错；确已损坏才重建，避免一次坏状态永久卡住。
			if stIsGitRepo(ctx, r.Dir) {
				syncErr = err
				break
			}
			if rmErr := os.RemoveAll(r.Dir); rmErr != nil {
				syncErr = fmt.Errorf("%w；清理损坏目录失败: %v", err, rmErr)
				break
			}
			syncErr = s.stClone(ctx, r)
		}
	}

	var head string
	if syncErr == nil {
		if out, err := stRunGit(ctx, r.Dir, "rev-parse", "HEAD"); err == nil {
			head = strings.TrimSpace(out)
		}
	}

	s.mu.Lock()
	st := s.status[r.Name]
	if st == nil {
		st = &stRepoStatus{}
		s.status[r.Name] = st
	}
	st.lastErr = syncErr
	if syncErr == nil {
		st.lastSync = time.Now()
		if head != "" {
			st.head = head
		}
	}
	s.mu.Unlock()

	return syncErr
}

// Head 返回缓存的 HEAD sha；缓存为空时惰性执行一次 rev-parse。
func (s *Store) Head(repo string) string {
	s.mu.RLock()
	st, ok := s.status[repo]
	var cached string
	if ok {
		cached = st.head
	}
	s.mu.RUnlock()
	if cached != "" {
		return cached
	}

	r, ok := s.byName[repo]
	if !ok || !r.HasCode {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := stRunGit(ctx, r.Dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(out)

	s.mu.Lock()
	if st2, ok2 := s.status[repo]; ok2 {
		st2.head = head
	}
	s.mu.Unlock()
	return head
}

// ---- Load ----

const (
	stMaxFileSize = 512 * 1024
	stMaxLines    = 20000
	stMaxWorkers  = 8
)

// stFileCandidate 是通过 include/exclude 与硬排除过滤后、待读取的文件。
type stFileCandidate struct {
	relPath string
	absPath string
}

// Load 用 `git ls-files -z` 枚举受版本控制的文件（天然跳过 .gitignore 与 .git），
// 依次套用硬排除、Include/Exclude 过滤，再以有界并发读取内容并按 ls-files 顺序稳定返回。
func (s *Store) Load(r *Repo) ([]File, error) {
	if r == nil {
		return nil, errors.New("repo.go: Load: nil repo")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := stRunGit(ctx, r.Dir, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("ls-files: %w", err)
	}

	var cands []stFileCandidate
	for _, raw := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if raw == "" {
			continue
		}
		rel, ok := stSafeRelPath(raw)
		if !ok || stIsHardExcluded(rel) {
			continue
		}
		if len(r.Include) > 0 {
			matched := false
			for _, g := range r.Include {
				if MatchGlob(g, rel) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		excluded := false
		for _, g := range r.Exclude {
			if MatchGlob(g, rel) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		cands = append(cands, stFileCandidate{
			relPath: rel,
			absPath: filepath.Join(r.Dir, filepath.FromSlash(rel)),
		})
	}

	results := make([]File, len(cands))
	keep := make([]bool, len(cands))

	workers := runtime.NumCPU()
	if workers > stMaxWorkers {
		workers = stMaxWorkers
	}
	if workers < 1 {
		workers = 1
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for i, c := range cands {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c stFileCandidate) {
			defer wg.Done()
			defer func() { <-sem }()
			f, ok := stLoadOneFile(r.Name, c)
			if ok {
				results[i] = f
				keep[i] = true
			}
		}(i, c)
	}
	wg.Wait()

	out2 := make([]File, 0, len(cands))
	for i, k := range keep {
		if k {
			out2 = append(out2, results[i])
		}
	}
	return out2, nil
}

// stLoadOneFile 读取单个候选文件，套用大小/二进制/行数限制，转换为 File。
func stLoadOneFile(repoName string, c stFileCandidate) (File, bool) {
	fi, err := os.Stat(c.absPath)
	if err != nil || fi.IsDir() || fi.Size() > stMaxFileSize {
		return File{}, false
	}
	data, err := os.ReadFile(c.absPath)
	if err != nil || IsProbablyBinary(data) {
		return File{}, false
	}

	rawLines := strings.Split(string(data), "\n")
	lines := make([]string, 0, len(rawLines))
	for _, ln := range rawLines {
		lines = append(lines, strings.TrimSuffix(ln, "\r"))
	}
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > stMaxLines {
		lines = lines[:stMaxLines]
	}

	return File{
		Repo:  repoName,
		Path:  c.relPath,
		Lang:  DetectLang(c.relPath),
		Lines: lines,
	}, true
}

// ---- Log ----

// stLogFieldSep / stLogRecSep 是 git log 自定义 --pretty=format 中使用的字段/记录分隔符，
// 均为不可打印控制字符，正常提交信息中不会出现，避免与提交标题/正文内容混淆。
const (
	stLogFieldSep = "\x1f"
	stLogRecSep   = "\x1e"
)

// Log 返回最近 n 条提交（默认 10，上限 50），可选按路径与 grep 关键字过滤。
//
// 实现拆成两条独立的 git log 调用而非把 --name-only 与自定义 --pretty 格式混在同一条命令里：
// 元数据用 "%H\x1f%an\x1f%ad\x1f%s\x1f%b\x1e" 分隔，可用 \x1e 干净切分，不受正文内换行/空行影响；
// 文件名用 "%H --name-only" 单独取，按 git 输出中提交间的空行切块解析。两条命令使用完全相同的
// n/grep/path 过滤条件，保证提交集合与顺序一一对应，再按 sha 关联，避免在一条命令里既要按控制符
// 切分字段、又要按空行切分文件名列表所带来的交叉歧义。
func (s *Store) Log(ctx context.Context, repo, path, grep string, n int) ([]Commit, error) {
	r, ok := s.byName[repo]
	if !ok {
		return nil, fmt.Errorf("unknown repo: %s", repo)
	}
	if !r.HasCode {
		return nil, fmt.Errorf("%s 为反馈仓库（无源码），不提供代码检索与提交历史", repo)
	}
	if n <= 0 {
		n = 10
	}
	if n > 50 {
		n = 50
	}

	var safePath string
	if path != "" {
		sp, ok := stSafeRelPath(path)
		if !ok {
			return nil, fmt.Errorf("invalid path: %s", path)
		}
		safePath = sp
	}

	format := "%H" + stLogFieldSep + "%an" + stLogFieldSep + "%ad" + stLogFieldSep + "%s" + stLogFieldSep + "%b" + stLogRecSep
	metaArgs := []string{"log", "-n", strconv.Itoa(n), "--date=short", "--pretty=format:" + format}
	nameArgs := []string{"log", "-n", strconv.Itoa(n), "--pretty=format:%H", "--name-only"}
	if grep != "" {
		metaArgs = append(metaArgs, "--grep="+grep, "-i")
		nameArgs = append(nameArgs, "--grep="+grep, "-i")
	}
	if safePath != "" {
		metaArgs = append(metaArgs, "--", safePath)
		nameArgs = append(nameArgs, "--", safePath)
	}

	metaOut, err := stRunGit(ctx, r.Dir, metaArgs...)
	if err != nil {
		return nil, err
	}
	if metaOut == "" {
		return nil, nil
	}

	var filesBySHA map[string][]string
	if nameOut, err := stRunGit(ctx, r.Dir, nameArgs...); err == nil {
		filesBySHA = stParseNameOnly(nameOut)
	}

	recs := strings.Split(metaOut, stLogRecSep)
	commits := make([]Commit, 0, len(recs))
	for _, rec := range recs {
		rec = strings.TrimPrefix(rec, "\n")
		if rec == "" {
			continue
		}
		fields := strings.SplitN(rec, stLogFieldSep, 5)
		if len(fields) < 5 {
			continue
		}
		body := strings.TrimRight(fields[4], "\n")
		if len(body) > 800 {
			body = body[:800]
		}
		files := filesBySHA[fields[0]]
		if len(files) > 20 {
			files = files[:20]
		}
		commits = append(commits, Commit{
			SHA:     fields[0],
			Author:  fields[1],
			Date:    fields[2],
			Subject: fields[3],
			Body:    body,
			Files:   files,
		})
	}
	return commits, nil
}

// stParseNameOnly 解析 `git log --pretty=format:%H --name-only` 的输出：提交之间以空行分隔，
// 每块首行为 sha，其余行为该提交改动的文件路径。
func stParseNameOnly(out string) map[string][]string {
	result := make(map[string][]string)
	if out == "" {
		return result
	}
	for _, block := range strings.Split(out, "\n\n") {
		block = strings.Trim(block, "\n")
		if block == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		sha := strings.TrimSpace(lines[0])
		if len(sha) != 40 {
			continue
		}
		var files []string
		for _, f := range lines[1:] {
			f = strings.TrimSpace(f)
			if f != "" {
				files = append(files, f)
			}
		}
		result[sha] = files
	}
	return result
}

// ---- Blame ----

// Blame 对文件的 [start,end] 行区间做逐行溯源；区间非法或跨度超过 400 行时收敛到 400 行以内。
func (s *Store) Blame(ctx context.Context, repo, path string, start, end int) ([]BlameLine, error) {
	r, ok := s.byName[repo]
	if !ok {
		return nil, fmt.Errorf("unknown repo: %s", repo)
	}
	if !r.HasCode {
		return nil, fmt.Errorf("%s 为反馈仓库（无源码），不提供代码检索与提交历史", repo)
	}
	safePath, ok := stSafeRelPath(path)
	if !ok {
		return nil, fmt.Errorf("invalid path: %s", path)
	}
	if start <= 0 {
		start = 1
	}
	if end < start || end-start > 400 {
		end = start + 400
	}

	out, err := stRunGit(ctx, r.Dir, "blame", "--line-porcelain", "-L", fmt.Sprintf("%d,%d", start, end), "--", safePath)
	if err != nil {
		return nil, err
	}
	return stParsePorcelainBlame(out), nil
}

// stParsePorcelainBlame 解析 `git blame --line-porcelain` 的输出。--line-porcelain 保证每一行都
// 携带完整的提交元数据（不像 --porcelain 那样对重复出现的提交做省略），因此逐行状态机足够：
// 遇到 "<40位十六进制sha> <origline> <finalline>[ <numlines>]" 形式的头部行开启新块，随后的
// "author "/"author-time " 行更新当前作者/日期，遇到以 制表符 开头的内容行即产出一条 BlameLine。
func stParsePorcelainBlame(out string) []BlameLine {
	var result []BlameLine
	var curSHA, curAuthor, curDate string
	var curFinalLine int

	for _, ln := range strings.Split(out, "\n") {
		switch {
		case stLooksLikeBlameHeader(ln):
			fields := strings.Fields(ln)
			curSHA = fields[0]
			if len(fields) >= 3 {
				if v, err := strconv.Atoi(fields[2]); err == nil {
					curFinalLine = v
				}
			}
		case strings.HasPrefix(ln, "author "):
			curAuthor = strings.TrimPrefix(ln, "author ")
		case strings.HasPrefix(ln, "author-time "):
			if v, err := strconv.ParseInt(strings.TrimPrefix(ln, "author-time "), 10, 64); err == nil {
				curDate = time.Unix(v, 0).UTC().Format("2006-01-02")
			}
		case strings.HasPrefix(ln, "\t"):
			result = append(result, BlameLine{
				Line:   curFinalLine,
				SHA:    curSHA,
				Author: curAuthor,
				Date:   curDate,
				Text:   ln[1:],
			})
		}
	}
	return result
}

// stLooksLikeBlameHeader 判断一行是否是 blame porcelain 的提交头部行：前 40 个字符是十六进制 sha，
// 紧跟一个空格。
func stLooksLikeBlameHeader(ln string) bool {
	if len(ln) < 41 || ln[40] != ' ' {
		return false
	}
	for i := 0; i < 40; i++ {
		c := ln[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
