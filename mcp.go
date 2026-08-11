// MCP 协议层：无状态 Streamable HTTP 传输 + JSON-RPC 2.0 分发。
//
// 为什么是无状态：本服务的每个工具调用都是纯查询，不存在跨调用的会话态。
// 因此不下发 Mcp-Session-Id，POST 一律以 application/json 单次响应；
// 客户端（LangBot）随时重连都能立刻用，也无需服务端维护长连接与会话回收。
// GET/DELETE 因此返回 405——无服务端主动推送，也无会话可终止。
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const (
	// 实现面向的协议版本；initialize 时若客户端声明了受支持的其它版本则回声该版本。
	protocolVersion = "2025-06-18"
	serverTitle     = "repo-mcp"
	serverVersion   = "0.1.0"

	maxRequestBytes = 1 << 20
)

// 本服务支持的协议版本集合，用于 initialize 协商与 MCP-Protocol-Version 头校验。
var supportedProtocols = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
	"2024-10-07": true,
	"2025-11-25": true,
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// authorize 校验 Bearer 令牌。Token 为空表示关闭鉴权。
func (s *Server) authorize(r *http.Request) bool {
	if s.cfg.Token == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	got, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		// 容忍大小写不同的 scheme。
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			got, ok = h[7:], true
		}
	}
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(s.cfg.Token)) == 1
}

// handleMCP 是唯一的 MCP 端点。
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !s.authorize(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="repo-mcp"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodPost:
	case http.MethodGet, http.MethodDelete:
		// 无状态实现：没有服务端主动推送流，也没有会话可删除。
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed: this server is stateless", http.StatusMethodNotAllowed)
		return
	case http.MethodOptions:
		w.Header().Set("Allow", "POST, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supportedProtocols[v] {
		http.Error(w, "unsupported MCP-Protocol-Version: "+v, http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxRequestBytes {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}

	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		writeRPC(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: codeInvalidRequest, Message: "empty request body"},
		})
		return
	}

	// JSON-RPC 批量在 2025-06-18 已移除，但旧版客户端仍可能发送数组，照单处理。
	if trimmed[0] == '[' {
		var batch []rpcRequest
		if err := json.Unmarshal([]byte(trimmed), &batch); err != nil {
			writeRPC(w, http.StatusBadRequest, rpcResponse{
				JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &rpcError{Code: codeParse, Message: "parse error: " + err.Error()},
			})
			return
		}
		out := make([]rpcResponse, 0, len(batch))
		for i := range batch {
			if resp, ok := s.dispatch(r.Context(), &batch[i]); ok {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, http.StatusOK, out)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
		writeRPC(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: codeParse, Message: "parse error: " + err.Error()},
		})
		return
	}
	resp, ok := s.dispatch(r.Context(), &req)
	if !ok {
		// 通知（无 id）没有响应体，按规范返回 202。
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, http.StatusOK, resp)
}

func writeRPC(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// dispatch 处理单条 JSON-RPC 消息。ok=false 表示这是通知，不应产生响应。
func (s *Server) dispatch(ctx context.Context, req *rpcRequest) (rpcResponse, bool) {
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"

	reply := func(result any, rerr *rpcError) (rpcResponse, bool) {
		if isNotification {
			return rpcResponse{}, false
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr}, true
	}

	switch req.Method {
	case "initialize":
		return reply(s.initializeResult(req.Params), nil)

	case "notifications/initialized", "notifications/cancelled", "notifications/roots/list_changed":
		return rpcResponse{}, false

	case "ping":
		return reply(map[string]any{}, nil)

	case "tools/list":
		defs := s.toolDefs()
		list := make([]map[string]any, 0, len(defs))
		for _, d := range defs {
			list = append(list, map[string]any{
				"name":        d.Name,
				"title":       d.Title,
				"description": d.Desc,
				"inputSchema": d.Schema,
			})
		}
		return reply(map[string]any{"tools": list}, nil)

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return reply(nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()})
			}
		}
		if p.Name == "" {
			return reply(nil, &rpcError{Code: codeInvalidParams, Message: "missing tool name"})
		}
		text, err := s.callTool(ctx, p.Name, p.Arguments)
		if errors.Is(err, errUnknownTool) {
			return reply(nil, &rpcError{Code: codeMethodNotFound, Message: err.Error()})
		}
		if err != nil {
			// 工具级失败按 MCP 约定走 isError 而非协议错误，让模型能读到原因并自行改写查询。
			return reply(map[string]any{
				"content": []map[string]any{{"type": "text", "text": "错误：" + err.Error()}},
				"isError": true,
			}, nil)
		}
		return reply(map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": false,
		}, nil)

	// 声明了 tools 之外的能力就必须能应答其列举方法，否则部分客户端会在启动期报错。
	case "resources/list":
		return reply(map[string]any{"resources": []any{}}, nil)
	case "resources/templates/list":
		return reply(map[string]any{"resourceTemplates": []any{}}, nil)
	case "prompts/list":
		return reply(map[string]any{"prompts": []any{}}, nil)

	default:
		return reply(nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method})
	}
}

func (s *Server) initializeResult(params json.RawMessage) map[string]any {
	version := protocolVersion
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(params, &p); err == nil && supportedProtocols[p.ProtocolVersion] {
			version = p.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    serverTitle,
			"title":   "仓库源码检索",
			"version": serverVersion,
		},
		"instructions": s.instructions(),
	}
}

// instructions 会被客户端注入到系统提示。IM 场景的小模型很依赖它来正确编排工具。
func (s *Server) instructions() string {
	var b strings.Builder
	b.WriteString("本服务可检索以下已索引仓库的源码，用于回答代码相关问题。\n\n可用仓库（repo 参数取这些短名）：\n")
	for _, r := range s.store.Repos() {
		b.WriteString("  - " + r.Name)
		if d := s.cfg.desc(r.Name); d != "" {
			b.WriteString("：" + d)
		}
		switch {
		case r.IssueWrite:
			b.WriteString("（issue：可查、可代提交与管理）")
		case r.IssueRead:
			b.WriteString("（issue：只可查）")
		}
		b.WriteString("\n")
	}
	b.WriteString(`
使用要求：
1. 涉及代码的问题必须先检索再回答，禁止凭记忆臆测实现细节。
2. 不清楚仓库结构时先调用 repo_overview 建立坐标，再检索。
3. 已知符号名（函数/类型/类）用 find_symbol；描述性问题用 search_code；
   要看完整实现用 read_file；问"为何这样改"用 git_history。
4. 回答必须引用来源，格式为 路径:行号，并附检索结果给出的链接。
5. 检索无结果时如实说明未找到，不要编造代码。
`)
	b.WriteString(`
issue / PR 查询（任意公开仓库，repo 参数可用配置短名或 owner/name 如 example-owner/AstrBot）：
6. 用户问「有没有人提过 / 这个功能什么进度」→ search_issues；要看细节与结论 → read_issue。
7. 能靠检索代码直接回答的问题就直接回答，不要开 issue。issue 只用于缺陷、异常与功能需求。
8. 用户问「有哪些 PR / 有人提 PR 吗 / PR 什么状态」→ search_pulls；详情 → read_pull；讨论 → list_pull_comments。
9. 查询类工具对任意公开仓库开放；写入与修改只对配置的可写仓库（或管理员指定）有效。
`)
	if len(s.issueRepos(false)) > 0 {
		b.WriteString(`10. 提交 issue 前必须两步齐全：先用 search_code / find_symbol 调研，再用 search_issues(state=all) 查重。
   调研结论无论「已确认」还是「未能确认」都要如实写进 create_issue 的 confidence 与 evidence，不要编造出处。
11. 一个问题只提一次。补充信息用 update_issue 追加评论，不要另开新 issue。
12. 不要主动关闭 issue。只有用户明确要求、或问题确已解决时才 close，并写清结论。
13. 写操作前先确认仓库对得上：把问题提到与之无关的仓库比不提更糟。
14. 追加评论（action=comment）仅管理员（adminReporters）可执行；管理员可对任意仓库（token 可访问的）写入与修改。
`)
	}
	if len(s.issueRepos(true)) > 0 {
		b.WriteString(`8. 提交 issue 前必须两步齐全：先用 search_code / find_symbol 调研，再用 search_issues(state=all) 查重。
   调研结论无论「已确认」还是「未能确认」都要如实写进 create_issue 的 confidence 与 evidence，不要编造出处。
9. 一个问题只提一次。补充信息用 update_issue 追加评论，不要另开新 issue。
10. 不要主动关闭 issue。只有用户明确要求、或问题确已解决时才 close，并写清结论。
11. 写操作前先确认仓库对得上：把问题提到与之无关的仓库比不提更糟。
`)
	}
	return b.String()
}

// handleRoot 兜底根路径。MCP 客户端配置里只填域名、漏掉 /mcp 是极常见的操作，
// 与其回一个无从下手的 404，不如直接按 MCP 处理；GET 则给出端点指引。
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, serverTitle+"/"+serverVersion+"\n"+
			"MCP 端点：POST /mcp（Bearer 鉴权，本路径 / 亦可直接使用）\n"+
			"健康检查：GET /healthz\n")
		return
	}
	s.handleMCP(w, r)
}

// handleHealth 供运维探活；不需要鉴权。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	stats := s.index.Stats()
	type repoHealth struct {
		Name     string `json:"name"`
		Head     string `json:"head"`
		Files    int    `json:"files"`
		Symbols  int    `json:"symbols"`
		Indexed  bool   `json:"indexed"`
		NoCode   bool   `json:"noCode,omitempty"` // 反馈仓库：无源码，不克隆不索引
		Issues   string `json:"issues"`           // off / read / write
		Slug     string `json:"slug,omitempty"`
		LastSync string `json:"lastSync,omitempty"`
		Error    string `json:"error,omitempty"`
	}
	out := struct {
		Server string       `json:"server"`
		Ready  bool         `json:"ready"`
		Repos  []repoHealth `json:"repos"`
	}{Server: serverTitle + "/" + serverVersion}

	ready := true
	for _, r := range s.store.Repos() {
		st, ok := stats[r.Name]
		h := repoHealth{
			Name: r.Name, Head: s.store.Head(r.Name), Files: st.Files, Symbols: st.Symbols,
			Indexed: ok, NoCode: !r.HasCode, Issues: issueMode(r), Slug: r.Slug,
		}
		_, last, lerr := s.store.Status(r.Name)
		if !last.IsZero() {
			h.LastSync = last.UTC().Format("2006-01-02T15:04:05Z")
		}
		if lerr != nil {
			h.Error = truncate(lerr.Error(), 200)
		}
		if !r.HasCode {
			// 反馈仓库无源码，不参与就绪判定。
			out.Repos = append(out.Repos, h)
			continue
		}
		if !ok {
			ready = false
		}
		out.Repos = append(out.Repos, h)
	}
	out.Ready = ready

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(out)
}

// truncate 按 rune 边界安全截断，避免把多字节字符切坏。
func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	r := []rune(s)
	// 先按字节预算粗切，再回退到 rune 边界。
	for len(string(r)) > max && len(r) > 0 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}
