// lang.go：语言识别（按扩展名/文件名）与自实现的 glob 匹配（支持 ** 跨目录通配）。
// DetectLang 与 MatchGlob 是跨模块共享的辅助函数契约，签名不可更改。
package main

import "strings"

// ixBasename 返回 "/" 分隔路径的最后一段。
func ixBasename(path string) string {
	if idx := strings.LastIndexByte(path, '/'); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// ixExtLower 返回 basename 中最后一个 "." 起的扩展名（含点，小写）；无扩展名返回 ""。
func ixExtLower(base string) string {
	idx := strings.LastIndexByte(base, '.')
	if idx < 0 || idx == 0 { // 隐藏文件如 ".gitignore" 不算扩展名
		return ""
	}
	return strings.ToLower(base[idx:])
}

// DetectLang 依据路径的扩展名（必要时按 basename）返回小写语言标识；未知返回 ""。
func DetectLang(path string) string {
	base := strings.ToLower(ixBasename(path))
	switch base {
	case "dockerfile":
		return "dockerfile"
	case "makefile", "gnumakefile", "makefile.am", "makefile.in":
		return "make"
	case "go.mod", "go.sum", "go.work":
		return "go"
	}
	switch ixExtLower(base) {
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".ts", ".mts", ".cts":
		return "typescript"
	case ".tsx":
		return "tsx"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "jsx"
	case ".py", ".pyi", ".pyw":
		return "python"
	case ".dart":
		return "dart"
	case ".java":
		return "java"
	case ".kt", ".kts":
		return "kotlin"
	case ".c", ".h":
		return "c"
	case ".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx", ".ino":
		return "cpp"
	case ".cs":
		return "csharp"
	case ".rb", ".rake", ".gemspec":
		return "ruby"
	case ".php":
		return "php"
	case ".swift":
		return "swift"
	case ".scala", ".sc":
		return "scala"
	case ".sh", ".bash", ".zsh":
		return "shell"
	case ".sql":
		return "sql"
	case ".html", ".htm":
		return "html"
	case ".css":
		return "css"
	case ".scss", ".sass":
		return "scss"
	case ".json", ".jsonc":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".xml":
		return "xml"
	case ".md", ".markdown":
		return "markdown"
	case ".proto":
		return "proto"
	case ".lua":
		return "lua"
	case ".vue":
		return "vue"
	case ".svelte":
		return "svelte"
	case ".mk":
		return "make"
	default:
		return ""
	}
}

// ixSingleSegMatch 对不含 "/" 的单个路径片段做 "*"/"?" 通配匹配。
// 采用经典的线性回溯算法（记录上一个 "*" 的位置以便失败后重试），
// 不使用正则拼接，避免转义与 ReDoS 风险。
func ixSingleSegMatch(pat, s string) bool {
	var pi, si int
	starIdx, starMatch := -1, 0
	for si < len(s) {
		if pi < len(pat) && (pat[pi] == '?' || pat[pi] == s[si]) {
			pi++
			si++
			continue
		}
		if pi < len(pat) && pat[pi] == '*' {
			starIdx = pi
			starMatch = si
			pi++
			continue
		}
		if starIdx != -1 {
			pi = starIdx + 1
			starMatch++
			si = starMatch
			continue
		}
		return false
	}
	for pi < len(pat) && pat[pi] == '*' {
		pi++
	}
	return pi == len(pat)
}

// ixSegMatch 对按 "/" 切分后的 pattern 段与 path 段做匹配；"**" 段可匹配 0..N 个 path 段
// （因此 "a/**/b" 同时匹配 "a/b" 与 "a/x/y/b"）。
func ixSegMatch(pats, paths []string) bool {
	for len(pats) > 0 {
		if pats[0] == "**" {
			if len(pats) == 1 {
				return true // 末尾的 ** 匹配任意剩余内容（含 0 个）
			}
			for i := 0; i <= len(paths); i++ {
				if ixSegMatch(pats[1:], paths[i:]) {
					return true
				}
			}
			return false
		}
		if len(paths) == 0 {
			return false
		}
		if !ixSingleSegMatch(pats[0], paths[0]) {
			return false
		}
		pats = pats[1:]
		paths = paths[1:]
	}
	return len(paths) == 0
}

// MatchGlob 判断 path 是否匹配 pattern。
// 语义："?" 匹配单个非 "/" 字符；"*" 匹配任意个非 "/" 字符；"**" 匹配任意字符（含 "/"）。
// pattern 不含 "/" 时，只与 path 的 basename 比较。pattern 为空返回 true。
func MatchGlob(pattern, path string) bool {
	if pattern == "" {
		return true
	}
	if !strings.Contains(pattern, "/") {
		return ixSingleSegMatch(pattern, ixBasename(path))
	}
	return ixSegMatch(strings.Split(pattern, "/"), strings.Split(path, "/"))
}
