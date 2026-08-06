// index.go：内存倒排索引 + BM25 排序 + 符号索引。实现 Indexer 契约。
// 并发模型：每仓库一个只读快照（repoIndex），Replace 在锁外构建新快照后在写锁下整体替换；
// 查询方法一律读锁，互不阻塞。
package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
)

var _ Indexer = (*Index)(nil)

// ixPosting 是倒排索引的一条记录：term 出现在 repo 内第 fileIdx 个文件的第 line 行，tf 次。
type ixPosting struct {
	fileIdx int
	line    int
	tf      int
}

// ixLineHit 用于在打分阶段临时汇总某个 term 在某文件内各命中行的词频。
type ixLineHit struct {
	line int
	tf   int
}

// ixRepoIndex 是单个仓库的不可变查询快照，由 Replace 整体构建后原子替换。
type ixRepoIndex struct {
	files       []File
	pathIndex   map[string]int // 相对路径 -> files 下标
	postings    map[string][]ixPosting
	termDocFreq map[string]int // term -> 出现该 term 的文件数（用于 idf）
	docLen      []int          // 每个文件的 token 总数（含重复），BM25 文档长度
	avgDocLen   float64
	symbols     []Symbol
	stats       RepoStats
}

// Index 是 Indexer 的内存实现：按仓库分桶的倒排索引 + 符号表。
type Index struct {
	mu    sync.RWMutex
	repos map[string]*ixRepoIndex
}

// NewIndex 构造一个空的 Index。
func NewIndex() *Index {
	return &Index{repos: make(map[string]*ixRepoIndex)}
}

// ---- 分词 ----

var ixAtomRe = regexp.MustCompile(`[A-Za-z0-9_]+`)

// ixSplitWords 把一个仅含字母/数字/下划线的 atom 拆分为大小写驼峰/连续大写/下划线边界的子词。
// 例："parseHTTPResponse" -> ["parse","HTTP","Response"]；"max_retry_count" -> ["max","retry","count"]。
func ixSplitWords(atom string) []string {
	runes := []rune(atom)
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = cur[:0]
		}
	}
	for i, r := range runes {
		switch {
		case r == '_':
			flush()
		case unicode.IsDigit(r):
			if len(cur) > 0 && !unicode.IsDigit(cur[len(cur)-1]) {
				flush()
			}
			cur = append(cur, r)
		case unicode.IsUpper(r):
			if len(cur) > 0 {
				prev := cur[len(cur)-1]
				nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
				if unicode.IsLower(prev) || unicode.IsDigit(prev) {
					flush()
				} else if unicode.IsUpper(prev) && nextLower {
					flush()
				}
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return words
}

// ixTokenize 把一行文本/查询串切分为小写 token：既保留拆分后的子词，也保留原始整词（小写），
// 单字符 token 一律丢弃。这是精确 query 召回（符号名、报错串）的核心。
func ixTokenize(s string) []string {
	atoms := ixAtomRe.FindAllString(s, -1)
	out := make([]string, 0, len(atoms)*2)
	for _, atom := range atoms {
		whole := strings.ToLower(atom)
		if len(whole) >= 2 {
			out = append(out, whole)
		}
		words := ixSplitWords(atom)
		if len(words) > 1 {
			for _, w := range words {
				lw := strings.ToLower(w)
				if len(lw) >= 2 {
					out = append(out, lw)
				}
			}
		}
	}
	return out
}

// ---- BM25 ----

const (
	ixBM25K1 = 1.2
	ixBM25B  = 0.75
)

func ixIDF(n, df int) float64 {
	if df <= 0 || n <= 0 {
		return 0
	}
	v := math.Log(1 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
	if v < 0 {
		return 0
	}
	return v
}

func ixBM25Term(tf float64, dl int, avgdl float64) float64 {
	if avgdl <= 0 {
		avgdl = 1
	}
	denom := tf + ixBM25K1*(1-ixBM25B+ixBM25B*float64(dl)/avgdl)
	if denom == 0 {
		return 0
	}
	return tf * (ixBM25K1 + 1) / denom
}

// ---- 噪声路径识别（测试/生成代码降权） ----

var ixNoisyPathMarkers = []string{"test", "spec", "generated", "vendor", "node_modules", ".g.dart", "_pb"}

func ixIsNoisyPath(path string) bool {
	lp := strings.ToLower(path)
	for _, m := range ixNoisyPathMarkers {
		if strings.Contains(lp, m) {
			return true
		}
	}
	return false
}

// ---- Replace：重建单仓快照 ----

func (ix *Index) Replace(repo string, files []File) {
	ri := &ixRepoIndex{
		files:     files,
		pathIndex: make(map[string]int, len(files)),
		postings:  make(map[string][]ixPosting),
		docLen:    make([]int, len(files)),
	}

	byLang := make(map[string]int)
	var totalLines, totalDocLen int
	var symbols []Symbol

	for fi, f := range files {
		ri.pathIndex[f.Path] = fi
		totalLines += len(f.Lines)
		if f.Lang != "" {
			byLang[f.Lang]++
		}

		lineCount := make(map[string]int) // term -> 该文件内已记录的行数，控制每 (file,term) 最多 32 行
		for li, line := range f.Lines {
			toks := ixTokenize(line)
			if len(toks) == 0 {
				continue
			}
			ri.docLen[fi] += len(toks)
			freq := make(map[string]int, len(toks))
			for _, t := range toks {
				freq[t]++
			}
			for term, tf := range freq {
				if lineCount[term] >= 32 {
					continue
				}
				lineCount[term]++
				ri.postings[term] = append(ri.postings[term], ixPosting{fileIdx: fi, line: li + 1, tf: tf})
			}
		}
		totalDocLen += ri.docLen[fi]

		symbols = append(symbols, ExtractSymbols(f)...)
	}
	ri.symbols = symbols

	termDocFreq := make(map[string]int, len(ri.postings))
	for term, plist := range ri.postings {
		seen := make(map[int]struct{})
		for _, p := range plist {
			seen[p.fileIdx] = struct{}{}
		}
		termDocFreq[term] = len(seen)
	}
	ri.termDocFreq = termDocFreq

	if len(files) > 0 {
		ri.avgDocLen = float64(totalDocLen) / float64(len(files))
	}
	ri.stats = RepoStats{Files: len(files), Lines: totalLines, Symbols: len(symbols), ByLang: byLang}

	ix.mu.Lock()
	if ix.repos == nil {
		ix.repos = make(map[string]*ixRepoIndex)
	}
	ix.repos[repo] = ri
	ix.mu.Unlock()
}

// ---- Search ----

func (ix *Index) Search(q SearchQuery) []Hit {
	k := q.K
	if k <= 0 {
		k = 8
	}
	if k > 30 {
		k = 30
	}

	phrase := strings.TrimSpace(q.Text)
	qtf := make(map[string]int)
	for _, t := range ixTokenize(q.Text) {
		qtf[t]++
	}

	ix.mu.RLock()
	defer ix.mu.RUnlock()

	var repoNames []string
	if q.Repo != "" {
		if _, ok := ix.repos[q.Repo]; ok {
			repoNames = []string{q.Repo}
		}
	} else {
		for name := range ix.repos {
			repoNames = append(repoNames, name)
		}
		sort.Strings(repoNames)
	}

	var all []Hit
	for _, name := range repoNames {
		all = append(all, ix.searchRepo(name, ix.repos[name], q, phrase, qtf)...)
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		if all[i].Repo != all[j].Repo {
			return all[i].Repo < all[j].Repo
		}
		return all[i].Path < all[j].Path
	})
	if len(all) > k {
		all = all[:k]
	}
	return all
}

// searchRepo 在单个仓库快照内计算候选文件得分，返回每文件至多一条 Hit（未截断/未全局排序）。
func (ix *Index) searchRepo(repoName string, ri *ixRepoIndex, q SearchQuery, phrase string, qtf map[string]int) []Hit {
	if ri == nil || len(ri.files) == 0 {
		return nil
	}

	n := len(ri.files)
	allowed := make([]bool, n)
	anyAllowed := false
	for fi, f := range ri.files {
		if q.Lang != "" && !strings.EqualFold(f.Lang, q.Lang) {
			continue
		}
		if q.PathGlob != "" && !MatchGlob(q.PathGlob, f.Path) {
			continue
		}
		allowed[fi] = true
		anyAllowed = true
	}
	if !anyAllowed {
		return nil
	}

	bm25Score := make([]float64, n)
	symScore := make([]float64, n)
	phraseScore := make([]float64, n)

	bestTermLine := make([]int, n)
	bestTermScore := make([]float64, n)
	bestTermWhy := make([]string, n)

	bestSymLine := make([]int, n)
	bestSymScore := make([]float64, n)
	bestSymWhy := make([]string, n)

	phraseLine := make([]int, n)

	// --- term / BM25 信号 ---
	for term, qf := range qtf {
		postings := ri.postings[term]
		if len(postings) == 0 {
			continue
		}
		df := ri.termDocFreq[term]
		idf := ixIDF(n, df)
		if idf <= 0 {
			continue
		}
		fileTF := make(map[int]int)
		fileLines := make(map[int][]ixLineHit)
		for _, p := range postings {
			if !allowed[p.fileIdx] {
				continue
			}
			fileTF[p.fileIdx] += p.tf
			fileLines[p.fileIdx] = append(fileLines[p.fileIdx], ixLineHit{line: p.line, tf: p.tf})
		}
		for fi, tf := range fileTF {
			bm25Score[fi] += idf * ixBM25Term(float64(tf), ri.docLen[fi], ri.avgDocLen) * float64(qf)
			for _, lh := range fileLines[fi] {
				s := idf * float64(lh.tf)
				if s > bestTermScore[fi] {
					bestTermScore[fi] = s
					bestTermLine[fi] = lh.line
					bestTermWhy[fi] = "term:" + term
				}
			}
		}
	}

	// --- 符号精确/前缀命中 ---
	if len(qtf) > 0 {
		for _, sym := range ri.symbols {
			fi, ok := ri.pathIndex[sym.Path]
			if !ok || !allowed[fi] {
				continue
			}
			lname := strings.ToLower(sym.Name)
			var bonus float64
			for t := range qtf {
				if lname == t {
					if 6.0 > bonus {
						bonus = 6.0
					}
				} else if strings.HasPrefix(lname, t) {
					if 2.0 > bonus {
						bonus = 2.0
					}
				}
			}
			if bonus <= 0 {
				continue
			}
			symScore[fi] += bonus
			if bonus > bestSymScore[fi] {
				bestSymScore[fi] = bonus
				bestSymLine[fi] = sym.Line
				bestSymWhy[fi] = "symbol:" + sym.Name
			}
		}
	}

	// --- 短语/原文子串命中（仅在已有其它信号的文件上扫描，控制成本） ---
	if len(phrase) >= 2 {
		lowerPhrase := strings.ToLower(phrase)
		for fi, f := range ri.files {
			if !allowed[fi] || (bm25Score[fi] <= 0 && symScore[fi] <= 0) {
				continue
			}
			for li, line := range f.Lines {
				if strings.Contains(strings.ToLower(line), lowerPhrase) {
					phraseScore[fi] = 4.0
					phraseLine[fi] = li + 1
					break
				}
			}
		}
	}

	// --- 覆盖率门槛 ---
	// 分词把 max_retry_count 拆成 max/retry/count 是精确 query 召回的关键，
	// 但代价是任何一个常见子词都能把毫不相关的文件拉进结果。
	// 因此要求：查询中的某个原始词必须被「整词命中」或「其全部子词同时命中」。
	// 例：查询 zzqqxx_not_exist_token 不会仅因为文件里出现 token 就命中。
	satisfied := make([]int, n)
	nAtoms := 0
	for _, atom := range ixAtomRe.FindAllString(q.Text, -1) {
		whole := strings.ToLower(atom)
		var subs []string
		for _, w := range ixSplitWords(atom) {
			if lw := strings.ToLower(w); len(lw) >= 2 && lw != whole {
				subs = append(subs, lw)
			}
		}
		if len(whole) < 2 && len(subs) == 0 {
			continue
		}
		nAtoms++

		hitWhole := make([]bool, n)
		if len(whole) >= 2 {
			ixMarkFiles(ri, whole, allowed, hitWhole)
		}
		var hitAllSubs []bool
		if len(subs) > 0 {
			hitAllSubs = make([]bool, n)
			for fi := range hitAllSubs {
				hitAllSubs[fi] = allowed[fi]
			}
			mark := make([]bool, n)
			for _, sub := range subs {
				clear(mark)
				ixMarkFiles(ri, sub, allowed, mark)
				for fi := range hitAllSubs {
					hitAllSubs[fi] = hitAllSubs[fi] && mark[fi]
				}
			}
		}
		for fi := 0; fi < n; fi++ {
			if hitWhole[fi] || (hitAllSubs != nil && hitAllSubs[fi]) {
				satisfied[fi]++
			}
		}
	}

	var hits []Hit
	for fi, f := range ri.files {
		if !allowed[fi] {
			continue
		}
		total := bm25Score[fi] + symScore[fi] + phraseScore[fi]
		if total <= 0 {
			continue
		}

		// 原文子串整段命中即视为完全覆盖：能匹配整段原文必然相关。
		cov := satisfied[fi]
		if phraseScore[fi] > 0 {
			cov = nAtoms
		}
		if nAtoms > 0 && cov == 0 {
			continue
		}

		// 路径加成
		lowerPath := strings.ToLower(f.Path)
		base := ixBasename(f.Path)
		lowerBase := strings.ToLower(base)
		baseNoExt := lowerBase
		if idx := strings.LastIndexByte(baseNoExt, '.'); idx > 0 {
			baseNoExt = baseNoExt[:idx]
		}
		lowerFullQuery := strings.ToLower(phrase)
		for t := range qtf {
			if strings.Contains(lowerPath, t) {
				total += 1.0
			}
		}
		if (lowerFullQuery != "" && (lowerBase == lowerFullQuery || baseNoExt == lowerFullQuery)) || func() bool {
			for t := range qtf {
				if lowerBase == t || baseNoExt == t {
					return true
				}
			}
			return false
		}() {
			total += 2.0
		}

		// 覆盖的查询词越多越靠前，避免多词查询里只命中一个词的结果压过全命中的。
		if nAtoms > 0 {
			total *= 0.5 + 0.5*float64(cov)/float64(nAtoms)
		}

		if ixIsNoisyPath(f.Path) {
			total *= 0.6
		}
		if total <= 0 {
			continue
		}

		line, why := 1, "term"
		switch {
		case bestSymScore[fi] > 0:
			line, why = bestSymLine[fi], bestSymWhy[fi]
		case phraseScore[fi] > 0:
			line, why = phraseLine[fi], "phrase"
		case bestTermScore[fi] > 0:
			line, why = bestTermLine[fi], bestTermWhy[fi]
		default:
			line = 1
		}

		hits = append(hits, Hit{
			Repo:    repoName,
			Path:    f.Path,
			Line:    line,
			EndLine: line,
			Score:   total,
			Snippet: ixBuildSnippet(f, line),
			Why:     why,
		})
	}
	return hits
}

// ixMarkFiles 把包含 term 的候选文件在 dst 中置位，供覆盖率门槛使用。
func ixMarkFiles(ri *ixRepoIndex, term string, allowed, dst []bool) {
	for _, p := range ri.postings[term] {
		if allowed[p.fileIdx] {
			dst[p.fileIdx] = true
		}
	}
}

// ixBuildSnippet 取命中行 ±3 行上下文，每行带行号前缀，整体不超过 1200 字符。
func ixBuildSnippet(f File, line int) string {
	if line < 1 {
		line = 1
	}
	start := line - 3
	if start < 1 {
		start = 1
	}
	end := line + 3
	if end > len(f.Lines) {
		end = len(f.Lines)
	}
	if end < start {
		return ""
	}
	var b strings.Builder
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%6d| %s\n", i, ixTruncate(f.Lines[i-1], 200))
	}
	return ixTruncate(strings.TrimRight(b.String(), "\n"), 1200)
}

// ---- FindSymbol ----

// ixNormalizeName 把标识符归一为「去分隔符的小写子词串」，用于跨命名风格匹配：
// aria2StatusStr 与 aria2_status_str 都归一为 aria2statusstr。
// 模型经常记不准命名风格（Dart camelCase / Rust snake_case / SQL 全大写），
// 而 find_symbol 是查定义的首选工具，不能对风格敏感。
func ixNormalizeName(s string) string {
	var b strings.Builder
	for _, atom := range ixAtomRe.FindAllString(s, -1) {
		for _, w := range ixSplitWords(atom) {
			b.WriteString(strings.ToLower(w))
		}
	}
	return b.String()
}

func (ix *Index) FindSymbol(name, kind, repo string, k int) []Symbol {
	if k <= 0 {
		k = 20
	}
	if k > 100 {
		k = 100
	}
	lname := strings.ToLower(name)
	nname := ixNormalizeName(name)
	lkind := strings.ToLower(kind)
	kindOK := func(symKind string) bool {
		if lkind == "" {
			return true
		}
		sk := strings.ToLower(symKind)
		if sk == lkind {
			return true
		}
		if lkind == "func" && sk == "method" {
			return true
		}
		return false
	}

	type scored struct {
		sym   Symbol
		rank  int
		noisy bool
	}

	ix.mu.RLock()
	defer ix.mu.RUnlock()

	var repoNames []string
	if repo != "" {
		repoNames = []string{repo}
	} else {
		for r := range ix.repos {
			repoNames = append(repoNames, r)
		}
		sort.Strings(repoNames)
	}

	var cands []scored
	for _, rn := range repoNames {
		ri, ok := ix.repos[rn]
		if !ok {
			continue
		}
		for _, s := range ri.symbols {
			if !kindOK(s.Kind) {
				continue
			}
			ln := strings.ToLower(s.Name)
			nn := ixNormalizeName(s.Name)
			var rank int
			switch {
			case ln == lname:
				rank = 0
			case nname != "" && nn == nname:
				rank = 1
			case strings.HasPrefix(ln, lname):
				rank = 2
			case nname != "" && strings.HasPrefix(nn, nname):
				rank = 3
			case strings.Contains(ln, lname):
				rank = 4
			case nname != "" && strings.Contains(nn, nname):
				rank = 5
			default:
				continue
			}
			cands = append(cands, scored{sym: s, rank: rank, noisy: ixIsNoisyPath(s.Path)})
		}
	}

	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].rank != cands[j].rank {
			return cands[i].rank < cands[j].rank
		}
		if cands[i].noisy != cands[j].noisy {
			return !cands[i].noisy
		}
		if cands[i].sym.Repo != cands[j].sym.Repo {
			return cands[i].sym.Repo < cands[j].sym.Repo
		}
		if cands[i].sym.Path != cands[j].sym.Path {
			return cands[i].sym.Path < cands[j].sym.Path
		}
		return cands[i].sym.Line < cands[j].sym.Line
	})
	if len(cands) > k {
		cands = cands[:k]
	}
	out := make([]Symbol, len(cands))
	for i, c := range cands {
		out[i] = c.sym
	}
	return out
}

// ---- Tree ----

func ixDirDepth(path string, depth int) string {
	dir := ""
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		dir = path[:idx]
	}
	if dir == "" {
		return "."
	}
	segs := strings.Split(dir, "/")
	if len(segs) > depth {
		segs = segs[:depth]
	}
	return strings.Join(segs, "/")
}

func (ix *Index) Tree(repo string, maxEntries int) []string {
	if maxEntries <= 0 {
		maxEntries = 60
	}

	ix.mu.RLock()
	defer ix.mu.RUnlock()

	ri, ok := ix.repos[repo]
	if !ok {
		return nil
	}

	type agg struct {
		count int
		langs map[string]int
	}
	dirs := make(map[string]*agg)
	for _, f := range ri.files {
		d := ixDirDepth(f.Path, 3)
		a := dirs[d]
		if a == nil {
			a = &agg{langs: make(map[string]int)}
			dirs[d] = a
		}
		a.count++
		if f.Lang != "" {
			a.langs[f.Lang]++
		}
	}

	type row struct {
		dir   string
		count int
		langs string
	}
	rows := make([]row, 0, len(dirs))
	for d, a := range dirs {
		names := make([]string, 0, len(a.langs))
		for l := range a.langs {
			names = append(names, l)
		}
		sort.Slice(names, func(i, j int) bool {
			if a.langs[names[i]] != a.langs[names[j]] {
				return a.langs[names[i]] > a.langs[names[j]]
			}
			return names[i] < names[j]
		})
		rows = append(rows, row{dir: d, count: a.count, langs: strings.Join(names, ",")})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].dir < rows[j].dir
	})
	if len(rows) > maxEntries {
		rows = rows[:maxEntries]
	}

	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprintf("%-24s%4d files  %s", r.dir+"/", r.count, r.langs)
	}
	return out
}

// ---- File ----

func (ix *Index) File(repo, path string) (File, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	ri, ok := ix.repos[repo]
	if !ok {
		return File{}, false
	}
	if fi, ok := ri.pathIndex[path]; ok {
		return ri.files[fi], true
	}

	suffix := "/" + path
	match := -1
	for i, f := range ri.files {
		if strings.HasSuffix(f.Path, suffix) {
			if match != -1 {
				return File{}, false // 后缀不唯一，拒绝猜测
			}
			match = i
		}
	}
	if match == -1 {
		return File{}, false
	}
	return ri.files[match], true
}

// ---- Stats ----

func (ix *Index) Stats() map[string]RepoStats {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	out := make(map[string]RepoStats, len(ix.repos))
	for name, ri := range ix.repos {
		cp := ri.stats
		cp.ByLang = make(map[string]int, len(ri.stats.ByLang))
		for l, c := range ri.stats.ByLang {
			cp.ByLang[l] = c
		}
		out[name] = cp
	}
	return out
}
