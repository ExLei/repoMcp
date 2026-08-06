// symbols.go：基于逐行正则规则表的启发式符号提取（非完整语法分析）。
// 所有正则均在包级 var 中一次性编译，ExtractSymbols 是热路径，不得在调用中编译正则。
package main

import (
	"regexp"
	"strings"
)

// ixMaxSymbolsPerFile 是单文件符号数上限，防止病态文件拖垮索引。
const ixMaxSymbolsPerFile = 2000

// ixSymRule 描述一条"行 -> 符号"的启发式规则。
type ixSymRule struct {
	re        *regexp.Regexp
	kind      string              // 静态 kind；kindGroup>0 时改为使用该分组内容（小写）
	nameGroup int                 // 捕获组序号：符号名
	kindGroup int                 // >0 时表示 kind 取自该捕获组（小写），用于 SQL 等一行多态语句
	raw       bool                // true 时用未 trim 的原始行匹配（用于要求缩进的规则）
	exclude   map[string]struct{} // 命中该集合中的名字（小写）时丢弃（用于排除 if/for 等关键字误报）
}

func ixKeywordSet(words ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(words))
	for _, w := range words {
		m[w] = struct{}{}
	}
	return m
}

var ixCtrlKeywords = ixKeywordSet("if", "for", "while", "switch", "catch", "else", "do", "try", "finally", "return", "function", "new", "synchronized", "sizeof", "case", "default")

// ---- Go ----

var (
	ixReGoFunc       = regexp.MustCompile(`^func\s+([A-Za-z_]\w*)\s*(?:\[[^\]]*\])?\s*\(`)
	ixReGoMethod     = regexp.MustCompile(`^func\s+\([^)]*\)\s+([A-Za-z_]\w*)\s*(?:\[[^\]]*\])?\s*\(`)
	ixReGoStruct     = regexp.MustCompile(`^type\s+([A-Za-z_]\w*)\s*(?:\[[^\]]*\])?\s+struct\b`)
	ixReGoInterface  = regexp.MustCompile(`^type\s+([A-Za-z_]\w*)\s*(?:\[[^\]]*\])?\s+interface\b`)
	ixReGoType       = regexp.MustCompile(`^type\s+([A-Za-z_]\w*)\s*(?:\[[^\]]*\])?\s*(?:=|\S)`)
	ixReGoConst      = regexp.MustCompile(`^const\s+([A-Za-z_]\w*)`)
	ixReGoVar        = regexp.MustCompile(`^var\s+([A-Za-z_]\w*)`)
	ixReGoBlockIdent = regexp.MustCompile(`^([A-Za-z_]\w*)`)
)

// ---- Rust ----

var (
	ixReRustFn        = regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?(?:default\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?(?:const\s+)?fn\s+([A-Za-z_]\w*)`)
	ixReRustStruct    = regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?struct\s+([A-Za-z_]\w*)`)
	ixReRustEnum      = regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?enum\s+([A-Za-z_]\w*)`)
	ixReRustTrait     = regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?(?:unsafe\s+)?trait\s+([A-Za-z_]\w*)`)
	ixReRustImplFor   = regexp.MustCompile(`^impl(?:<[^>]*>)?\s+[A-Za-z_][\w:<>,\s]*?\s+for\s+([A-Za-z_][\w:]*)`)
	ixReRustImplPlain = regexp.MustCompile(`^impl(?:<[^>]*>)?\s+([A-Za-z_][\w:]*)`)
	ixReRustType      = regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?type\s+([A-Za-z_]\w*)`)
	ixReRustMacro     = regexp.MustCompile(`^macro_rules!\s+([A-Za-z_]\w*)`)
	ixReRustStatic    = regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?static\s+(?:mut\s+)?([A-Za-z_]\w*)`)
	ixReRustConst     = regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?const\s+([A-Za-z_]\w*)`)
)

// ---- JS/TS 系 ----

var (
	ixReJSFunction   = regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s+([A-Za-z_$][\w$]*)`)
	ixReJSClass      = regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)
	ixReJSInterface  = regexp.MustCompile(`^(?:export\s+)?interface\s+([A-Za-z_$][\w$]*)`)
	ixReJSType       = regexp.MustCompile(`^(?:export\s+)?type\s+([A-Za-z_$][\w$]*)\s*=`)
	ixReJSArrowParen = regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?const\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s*)?\(`)
	ixReJSArrowBare  = regexp.MustCompile(`^(?:export\s+)?const\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?[A-Za-z_$][\w$]*\s*=>`)
	ixReJSMethod     = regexp.MustCompile(`^[ \t]+([A-Za-z_$][\w$]*)\s*\(([^()]*)\)\s*\{\s*$`)
)

// ---- Python ----

var (
	ixRePyDef   = regexp.MustCompile(`^(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`)
	ixRePyClass = regexp.MustCompile(`^class\s+([A-Za-z_]\w*)`)
)

// ---- Dart ----

var (
	ixReDartClass     = regexp.MustCompile(`^(?:abstract\s+)?class\s+([A-Za-z_$]\w*)`)
	ixReDartMixin     = regexp.MustCompile(`^mixin\s+([A-Za-z_$]\w*)`)
	ixReDartExtension = regexp.MustCompile(`^extension\s+([A-Za-z_$]\w*)\b`)
	ixReDartEnum      = regexp.MustCompile(`^enum\s+([A-Za-z_$]\w*)`)
	ixReDartMethod    = regexp.MustCompile(`^(?:static\s+)?[A-Za-z_$][\w$.<>,\[\]? ]*[\s*]([A-Za-z_$]\w*)\s*\(([^()]*)\)\s*(?:async\*?|sync\*?)?\s*\{\s*$`)
)

// ---- JVM/CLR/其它单函数语言族 ----

var (
	ixReJavaClass     = regexp.MustCompile(`^(?:[\w]+\s+)*class\s+([A-Za-z_]\w*)`)
	ixReJavaInterface = regexp.MustCompile(`^(?:[\w]+\s+)*interface\s+([A-Za-z_]\w*)`)
	ixReJavaEnum      = regexp.MustCompile(`^(?:[\w]+\s+)*enum\s+([A-Za-z_]\w*)`)
	ixReJavaMethod    = regexp.MustCompile(`^(?:[\w<>\[\],.]+\s+)+([A-Za-z_]\w*)\s*\(([^()]*)\)\s*(?:throws\s+[\w,\s]+)?\{\s*$`)

	ixReKotlinClass     = regexp.MustCompile(`^(?:[\w]+\s+)*class\s+([A-Za-z_]\w*)`)
	ixReKotlinInterface = regexp.MustCompile(`^(?:[\w]+\s+)*interface\s+([A-Za-z_]\w*)`)
	ixReKotlinFun       = regexp.MustCompile(`^(?:[\w]+\s+)*fun\s+(?:<[^>]*>\s*)?(?:[A-Za-z_][\w.<>]*\.)?([A-Za-z_]\w*)\s*\(`)

	ixReCSharpClass     = regexp.MustCompile(`^(?:[\w]+\s+)*class\s+([A-Za-z_]\w*)`)
	ixReCSharpInterface = regexp.MustCompile(`^(?:[\w]+\s+)*interface\s+([A-Za-z_]\w*)`)
	ixReCSharpEnum      = regexp.MustCompile(`^(?:[\w]+\s+)*enum\s+([A-Za-z_]\w*)`)
	ixReCSharpMethod    = regexp.MustCompile(`^(?:[\w<>\[\],.]+\s+)+([A-Za-z_]\w*)\s*\(([^()]*)\)\s*\{\s*$`)

	ixReSwiftClass    = regexp.MustCompile(`^(?:public\s+|private\s+|internal\s+|final\s+|open\s+)*class\s+([A-Za-z_]\w*)`)
	ixReSwiftStruct   = regexp.MustCompile(`^(?:public\s+|private\s+|internal\s+)*struct\s+([A-Za-z_]\w*)`)
	ixReSwiftEnum     = regexp.MustCompile(`^(?:public\s+|private\s+|internal\s+)*enum\s+([A-Za-z_]\w*)`)
	ixReSwiftProtocol = regexp.MustCompile(`^(?:public\s+|private\s+|internal\s+)*protocol\s+([A-Za-z_]\w*)`)
	ixReSwiftFunc     = regexp.MustCompile(`^(?:public\s+|private\s+|internal\s+|static\s+|override\s+|final\s+|mutating\s+)*func\s+([A-Za-z_]\w*)`)

	ixReScalaClass  = regexp.MustCompile(`^(?:[\w]+\s+)*class\s+([A-Za-z_]\w*)`)
	ixReScalaTrait  = regexp.MustCompile(`^(?:[\w]+\s+)*trait\s+([A-Za-z_]\w*)`)
	ixReScalaObject = regexp.MustCompile(`^(?:[\w]+\s+)*object\s+([A-Za-z_]\w*)`)
	ixReScalaDef    = regexp.MustCompile(`^(?:[\w]+\s+)*def\s+([A-Za-z_]\w*)`)

	ixRePhpClass     = regexp.MustCompile(`^(?:abstract\s+|final\s+)?class\s+([A-Za-z_]\w*)`)
	ixRePhpInterface = regexp.MustCompile(`^interface\s+([A-Za-z_]\w*)`)
	ixRePhpTrait     = regexp.MustCompile(`^trait\s+([A-Za-z_]\w*)`)
	ixRePhpFunction  = regexp.MustCompile(`^(?:(?:public|private|protected|static)\s+)*function\s+&?([A-Za-z_]\w*)\s*\(`)

	ixReRubyClass  = regexp.MustCompile(`^class\s+([A-Za-z_]\w*)`)
	ixReRubyModule = regexp.MustCompile(`^module\s+([A-Za-z_]\w*)`)
	ixReRubyDef    = regexp.MustCompile(`^def\s+(?:self\.)?([A-Za-z_][\w?!=]*)`)
)

// ---- C/C++ ----

var (
	ixReCStruct  = regexp.MustCompile(`^(?:typedef\s+)?struct\s+([A-Za-z_]\w*)`)
	ixReCppClass = regexp.MustCompile(`^(?:template\s*<[^>]*>\s*)?class\s+([A-Za-z_]\w*)`)
	ixReCEnum    = regexp.MustCompile(`^(?:typedef\s+)?enum(?:\s+class)?\s+([A-Za-z_]\w*)`)
	ixReCTypedef = regexp.MustCompile(`^typedef\s+[\w \*<>,:]+?\s+([A-Za-z_]\w*)\s*;`)
	ixReCFunc    = regexp.MustCompile(`^(?:static\s+|inline\s+|extern\s+|virtual\s+)*[A-Za-z_][\w:<>*&,\s]*[\s*&]([A-Za-z_]\w*)\s*\(([^()]*)\)\s*(?:const\s*)?\{\s*$`)
)

// ---- SQL ----

var ixReSQLCreate = regexp.MustCompile(`(?i)^CREATE\s+(?:OR\s+REPLACE\s+)?(TABLE|VIEW|INDEX|FUNCTION|PROCEDURE|TRIGGER)\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z0-9_."\[\]]+)`)

// ---- Protobuf ----

var (
	ixReProtoMessage = regexp.MustCompile(`^message\s+([A-Za-z_]\w*)`)
	ixReProtoService = regexp.MustCompile(`^service\s+([A-Za-z_]\w*)`)
	ixReProtoRPC     = regexp.MustCompile(`^rpc\s+([A-Za-z_]\w*)\s*\(`)
	ixReProtoEnum    = regexp.MustCompile(`^enum\s+([A-Za-z_]\w*)`)
)

// ixSymRules 按语言索引的规则集合，顺序即优先级（每行取第一条命中的规则）。
var ixSymRules = map[string][]ixSymRule{
	"go": {
		{re: ixReGoMethod, kind: "method", nameGroup: 1},
		{re: ixReGoFunc, kind: "func", nameGroup: 1},
		{re: ixReGoStruct, kind: "struct", nameGroup: 1},
		{re: ixReGoInterface, kind: "interface", nameGroup: 1},
		{re: ixReGoType, kind: "type", nameGroup: 1},
		{re: ixReGoConst, kind: "const", nameGroup: 1},
		{re: ixReGoVar, kind: "var", nameGroup: 1},
	},
	"rust": {
		{re: ixReRustFn, kind: "func", nameGroup: 1},
		{re: ixReRustStruct, kind: "struct", nameGroup: 1},
		{re: ixReRustEnum, kind: "enum", nameGroup: 1},
		{re: ixReRustTrait, kind: "trait", nameGroup: 1},
		{re: ixReRustImplFor, kind: "impl", nameGroup: 1},
		{re: ixReRustImplPlain, kind: "impl", nameGroup: 1},
		{re: ixReRustType, kind: "type", nameGroup: 1},
		{re: ixReRustMacro, kind: "macro", nameGroup: 1},
		{re: ixReRustStatic, kind: "var", nameGroup: 1},
		{re: ixReRustConst, kind: "const", nameGroup: 1},
	},
	"python": {
		{re: ixRePyDef, kind: "func", nameGroup: 1},
		{re: ixRePyClass, kind: "class", nameGroup: 1},
	},
	"dart": {
		{re: ixReDartClass, kind: "class", nameGroup: 1},
		{re: ixReDartMixin, kind: "mixin", nameGroup: 1},
		{re: ixReDartExtension, kind: "extension", nameGroup: 1},
		{re: ixReDartEnum, kind: "enum", nameGroup: 1},
		{re: ixReDartMethod, kind: "method", nameGroup: 1, exclude: ixCtrlKeywords},
	},
	"java": {
		{re: ixReJavaClass, kind: "class", nameGroup: 1},
		{re: ixReJavaInterface, kind: "interface", nameGroup: 1},
		{re: ixReJavaEnum, kind: "enum", nameGroup: 1},
		{re: ixReJavaMethod, kind: "method", nameGroup: 1, exclude: ixCtrlKeywords},
	},
	"kotlin": {
		{re: ixReKotlinClass, kind: "class", nameGroup: 1},
		{re: ixReKotlinInterface, kind: "interface", nameGroup: 1},
		{re: ixReKotlinFun, kind: "func", nameGroup: 1},
	},
	"csharp": {
		{re: ixReCSharpClass, kind: "class", nameGroup: 1},
		{re: ixReCSharpInterface, kind: "interface", nameGroup: 1},
		{re: ixReCSharpEnum, kind: "enum", nameGroup: 1},
		{re: ixReCSharpMethod, kind: "method", nameGroup: 1, exclude: ixCtrlKeywords},
	},
	"swift": {
		{re: ixReSwiftClass, kind: "class", nameGroup: 1},
		{re: ixReSwiftStruct, kind: "struct", nameGroup: 1},
		{re: ixReSwiftEnum, kind: "enum", nameGroup: 1},
		{re: ixReSwiftProtocol, kind: "interface", nameGroup: 1},
		{re: ixReSwiftFunc, kind: "func", nameGroup: 1},
	},
	"scala": {
		{re: ixReScalaClass, kind: "class", nameGroup: 1},
		{re: ixReScalaTrait, kind: "trait", nameGroup: 1},
		{re: ixReScalaObject, kind: "object", nameGroup: 1},
		{re: ixReScalaDef, kind: "func", nameGroup: 1},
	},
	"php": {
		{re: ixRePhpClass, kind: "class", nameGroup: 1},
		{re: ixRePhpInterface, kind: "interface", nameGroup: 1},
		{re: ixRePhpTrait, kind: "trait", nameGroup: 1},
		{re: ixRePhpFunction, kind: "func", nameGroup: 1},
	},
	"ruby": {
		{re: ixReRubyClass, kind: "class", nameGroup: 1},
		{re: ixReRubyModule, kind: "module", nameGroup: 1},
		{re: ixReRubyDef, kind: "func", nameGroup: 1},
	},
	"c": {
		{re: ixReCStruct, kind: "struct", nameGroup: 1},
		{re: ixReCEnum, kind: "enum", nameGroup: 1},
		{re: ixReCTypedef, kind: "type", nameGroup: 1},
		{re: ixReCFunc, kind: "func", nameGroup: 1, exclude: ixCtrlKeywords},
	},
	"cpp": {
		{re: ixReCStruct, kind: "struct", nameGroup: 1},
		{re: ixReCppClass, kind: "class", nameGroup: 1},
		{re: ixReCEnum, kind: "enum", nameGroup: 1},
		{re: ixReCTypedef, kind: "type", nameGroup: 1},
		{re: ixReCFunc, kind: "func", nameGroup: 1, exclude: ixCtrlKeywords},
	},
	"sql": {
		{re: ixReSQLCreate, kind: "", nameGroup: 2, kindGroup: 1},
	},
	"proto": {
		{re: ixReProtoMessage, kind: "message", nameGroup: 1},
		{re: ixReProtoService, kind: "service", nameGroup: 1},
		{re: ixReProtoRPC, kind: "rpc", nameGroup: 1},
		{re: ixReProtoEnum, kind: "enum", nameGroup: 1},
	},
	"typescript": ixJSFamilyRules(),
	"tsx":        ixJSFamilyRules(),
	"javascript": ixJSFamilyRules(),
	"jsx":        ixJSFamilyRules(),
}

// ixJSFamilyRules 返回 JS/TS 系共用的规则切片（每次调用返回独立切片，避免跨语言共享底层数组被误改）。
func ixJSFamilyRules() []ixSymRule {
	return []ixSymRule{
		{re: ixReJSFunction, kind: "func", nameGroup: 1},
		{re: ixReJSClass, kind: "class", nameGroup: 1},
		{re: ixReJSInterface, kind: "interface", nameGroup: 1},
		{re: ixReJSType, kind: "type", nameGroup: 1},
		{re: ixReJSArrowParen, kind: "func", nameGroup: 1},
		{re: ixReJSArrowBare, kind: "func", nameGroup: 1},
		{re: ixReJSMethod, kind: "method", nameGroup: 1, raw: true, exclude: ixCtrlKeywords},
	}
}

// ixIsCommentLine 判断 trim 后的行是否是明显的注释行（避免把注释里的伪代码当定义）。
func ixIsCommentLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "//") ||
		strings.HasPrefix(trimmed, "#") ||
		strings.HasPrefix(trimmed, "*") ||
		strings.HasPrefix(trimmed, "--")
}

// ixCommentPrefix 返回该行使用的文档注释前缀；不是注释行则返回 ""。
func ixCommentPrefix(trimmed string) string {
	switch {
	case strings.HasPrefix(trimmed, "///"):
		return "///"
	case strings.HasPrefix(trimmed, "//"):
		return "//"
	case strings.HasPrefix(trimmed, "/**"):
		return "/**"
	case strings.HasPrefix(trimmed, "*"):
		return "*"
	case strings.HasPrefix(trimmed, "#"):
		return "#"
	case strings.HasPrefix(trimmed, `"""`):
		return `"""`
	default:
		return ""
	}
}

// ixTruncate 按 rune 截断字符串到 max 个字符，超出则追加省略号。
func ixTruncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// ixSignature 取该行 trim 后的内容，超过 200 字符截断。
func ixSignature(line string) string {
	return ixTruncate(strings.TrimSpace(line), 200)
}

// ixIsAttrLine 判断是否为属性/装饰器行（Rust #[...]、JVM/Python/TS 的 @Xxx、C# 的 [Attr]）。
// 它们常夹在文档注释与定义之间，收集文档时必须穿越，
// 既不能收录（Rust 的 #[derive] 不是注释），也不能就此中断（否则真正的文档注释取不到）。
func ixIsAttrLine(lang, t string) bool {
	switch lang {
	case "rust":
		return strings.HasPrefix(t, "#[") || strings.HasPrefix(t, "#![")
	case "python", "java", "kotlin", "scala", "typescript", "tsx", "javascript", "jsx", "dart", "php":
		return len(t) > 1 && t[0] == '@'
	case "csharp":
		return strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]")
	}
	return false
}

// ixCollectDoc 向上收集紧邻定义行的连续文档注释（中间不允许空行）。
// 保留的是文档的开头而非结尾：首行通常是摘要，而尾部往往是代码示例，
// 截取尾部会给出一段无头无尾的片段，对模型是误导。
func ixCollectDoc(lang string, lines []string, defIdx int) string {
	// 扫描窗口必须能覆盖整段文档，否则「取开头」取到的仍是中段。
	// 遇到非注释行即刻停止，因此普通代码只会向上看几行。
	const maxScan = 200
	const maxKeep = 8

	var collected []string
	for i := defIdx - 1; i >= 0 && len(collected) < maxScan; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			break
		}
		if ixIsAttrLine(lang, t) {
			continue
		}
		prefix := ixCommentPrefix(t)
		if prefix == "" {
			break
		}
		collected = append(collected, strings.TrimSpace(strings.TrimPrefix(t, prefix)))
	}
	for l, r := 0, len(collected)-1; l < r; l, r = l+1, r-1 {
		collected[l], collected[r] = collected[r], collected[l]
	}

	truncated := false
	if len(collected) > maxKeep {
		collected = collected[:maxKeep]
		truncated = true
	}
	doc := strings.Join(collected, "\n")
	if truncated {
		doc += "\n…"
	}
	return ixTruncate(doc, 400)
}

// ExtractSymbols 基于逐行正则规则表做启发式定义提取；未知语言返回 nil。
func ExtractSymbols(f File) []Symbol {
	rules, ok := ixSymRules[f.Lang]
	if !ok {
		return nil
	}

	var out []Symbol
	inBlock := "" // go 专用：当前是否处于 "const (" / "var (" 块内

	for i, line := range f.Lines {
		if len(out) >= ixMaxSymbolsPerFile {
			break
		}
		trimmed := strings.TrimSpace(line)

		if f.Lang == "go" && inBlock != "" {
			if trimmed == ")" {
				inBlock = ""
				continue
			}
			if trimmed != "" && !ixIsCommentLine(trimmed) {
				if m := ixReGoBlockIdent.FindStringSubmatch(trimmed); m != nil && m[1] != "_" {
					out = append(out, Symbol{
						Repo: f.Repo, Path: f.Path, Line: i + 1,
						Kind: inBlock, Name: m[1],
						Signature: ixSignature(line),
						Doc:       ixCollectDoc(f.Lang, f.Lines, i),
					})
				}
			}
			continue
		}
		if f.Lang == "go" {
			if trimmed == "const (" {
				inBlock = "const"
				continue
			}
			if trimmed == "var (" {
				inBlock = "var"
				continue
			}
		}

		if trimmed == "" || ixIsCommentLine(trimmed) {
			continue
		}

		for _, r := range rules {
			target := trimmed
			if r.raw {
				target = line
			}
			m := r.re.FindStringSubmatch(target)
			if m == nil || r.nameGroup >= len(m) {
				continue
			}
			name := m[r.nameGroup]
			if name == "" {
				continue
			}
			if r.exclude != nil {
				if _, bad := r.exclude[strings.ToLower(name)]; bad {
					continue
				}
			}
			kind := r.kind
			if r.kindGroup > 0 && r.kindGroup < len(m) {
				kind = strings.ToLower(m[r.kindGroup])
			}
			out = append(out, Symbol{
				Repo: f.Repo, Path: f.Path, Line: i + 1,
				Kind: kind, Name: name,
				Signature: ixSignature(line),
				Doc:       ixCollectDoc(f.Lang, f.Lines, i),
			})
			break
		}
	}
	return out
}
