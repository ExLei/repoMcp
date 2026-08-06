# repoMcp

给 **LangBot** 用的仓库源码检索 MCP 服务：让 IM 机器人里的 LLM 能查你的源码，并带着可核验的出处回答问题。

- 传输：**无状态 Streamable HTTP**，单端点 `POST /mcp`，Bearer 鉴权。
- 检索：本地 clone + 内存倒排索引（BM25）+ 正则符号表。**不需要 embedding、向量库或任何外部服务**。
- 依赖：**零第三方 Go 依赖**，`CGO_ENABLED=0` 单二进制。运行期只要求宿主有 `git`。

## 设计取舍

消费方是 IM 里的**小模型**：上下文窄、通常只肯调 1–3 次工具。这与 coding agent 完全不同，因此：

| 决定 | 原因 |
|---|---|
| 一次调用给足证据，不指望多轮迭代 | 小模型不会像 coding agent 那样反复 grep 收敛 |
| 输出是紧凑纯文本，不是 JSON | JSON 包装只增加 token，对模型阅读无收益 |
| 每条结果带 `路径:行号` + 钉住 commit 的 permalink | 答案必须能被人工核验，否则代码问答没有价值 |
| 硬字节预算（`maxResponseBytes`） | LangBot **不截断** tool 返回，服务端必须自己收口 |
| 只有 5 个工具 | 工具越多，小模型选错的概率越高 |
| 词法 + 符号表，不做向量检索 | 代码检索里标识符精确匹配的召回远超 embedding；向量的复杂度换不来相应收益 |

## 工具

| 工具 | 用途 |
|---|---|
| `repo_overview` | 技术栈、规模、目录结构、README 摘要。**让模型先建立坐标再检索** |
| `search_code` | 关键词检索，返回带行号与链接的片段。支持 `repo` / `lang` / `path_glob` 过滤 |
| `read_file` | 按行范围读原文（单次上限 400 行）。`blame: true` 可为每行附加最后修改的提交与作者 |
| `find_symbol` | 按名字查定义，返回签名与文档注释。已知符号名时比 `search_code` 精确 |
| `git_history` | 提交历史，回答「为什么这么写」「什么时候加的」 |

服务在 `initialize` 时会下发 `instructions`，向模型声明可用仓库、工具选择规则，以及**必须引用来源、检索无果时不得编造**。

## 配置

复制 `config.example.json` 为 `config.json`：

```json
{
  "listen": ":8790",
  "token": "长随机串",
  "dataDir": "./data",
  "syncInterval": "15m",
  "maxResponseBytes": 12000,
  "repos": [
    {
      "name": "fluxdown",
      "desc": "多协议下载器主仓（这句话会展示给模型，帮它选对仓库）",
      "url": "https://github.com/zerx-lab/FluxDown.git",
      "ref": "main",
      "webBase": "https://github.com/zerx-lab/FluxDown",
      "exclude": ["packages/**"]
    }
  ]
}
```

| 字段 | 说明 |
|---|---|
| `token` | Bearer 令牌。留空则**不鉴权**，仅可用于 `127.0.0.1` |
| `dataDir` | 各仓 clone 的存放目录，默认 `./data` |
| `syncInterval` | 后台 fetch + 重建索引周期，`"0"` 关闭 |
| `gitTimeout` | 单条 git 命令超时，默认 `3m` |
| `maxResponseBytes` | 单次工具返回的字节预算，默认 12000 |
| `repos[].name` | 短名，模型用它作 `repo` 参数。限 `^[a-z0-9][a-z0-9._-]{0,63}$` |
| `repos[].desc` | 一句话说明，出现在 `instructions` 与 `repo_overview` 里 |
| `repos[].webBase` | permalink 前缀。留空则从 `url` 推导（会剥掉内嵌凭据） |
| `repos[].include` / `exclude` | 通配符过滤，支持 `*`（不跨 `/`）、`**`（跨 `/`）、`?`；不含 `/` 的模式匹配文件名 |
| `repos[].dir` | 覆盖本地路径。**不要指向你的开发工作树**——同步会 `reset --hard` + `clean -fd` |

环境变量可覆盖：`REPOMCP_CONFIG` / `REPOMCP_LISTEN` / `REPOMCP_TOKEN` / `REPOMCP_DATA`。

私有仓：在 `url` 中内嵌 token（如 `https://x-access-token:<PAT>@github.com/owner/repo.git`），或在宿主上预先配好凭据助手。服务已强制 `GIT_TERMINAL_PROMPT=0`，凭据缺失会直接失败而不是挂起等待输入。

## 运行

```bash
cd repoMcp
go run . -config config.json      # 本机
task build                        # 交叉编译 linux-amd64 到 build/repomcp
```

启动后服务立即可用，首次 clone 与索引在后台进行；未就绪时工具会明确回复「索引进行中」而非空结果。

探活：`GET /healthz`（无需鉴权），返回每个仓的 HEAD、文件数、符号数、上次同步时间与错误。全部仓库就绪前返回 `503`。

## 接入 LangBot

LangBot 的 MCP 配置里新增一个 HTTP server：

```json
{
  "name": "repo",
  "mode": "http",
  "url": "http://127.0.0.1:8790/mcp",
  "headers": { "Authorization": "Bearer 你的token" },
  "timeout": 15,
  "tool_call_timeout_sec": 60
}
```

- `mode` 用 `http`（Streamable HTTP）。也可用 `remote` 让 LangBot 自动探测，但会多一次协商。
- **`Authorization` 的值必须带 `Bearer ` 前缀**，只填裸 token 会得到 401。
- URL 建议写全 `/mcp`。只填域名也能用（根路径做了兜底），但写全更明确。
- `timeout` 是连接与初始化超时；`tool_call_timeout_sec` 是单次工具调用超时，`git_history` 在大仓上可能偏慢，建议 ≥ 60。
- 配置完成后在聊天里发 `!func` 可确认 5 个工具已注册。
- 最后把这个 MCP server 绑定到目标流水线，模型才会看到这些工具。

排障顺序：先 `curl <地址>/healthz` 看 `ready` 与每仓 `error`（此接口不需要鉴权），
再用下面的探针验证 MCP 握手，最后才怀疑 LangBot 配置。

**安全**：本服务只读，不执行仓库中的任何代码，也不接受任意路径读取（`read_file` 只能读已索引的受版本控制文件，并拒绝 `..` 与绝对路径）。但它会把私有仓源码送进 LLM——请确认所用模型的数据策略，并把服务绑定在内网或 `127.0.0.1`。

## 检索语义

两条规则决定了结果质量，值得知道：

**覆盖率门槛。** 索引会把 `max_retry_count` 拆成 `max`/`retry`/`count`，这是精确查询能命中的关键；
代价是任何一个常见子词都可能把无关文件拉进结果。因此要求：查询中的某个原始词必须
**整词命中**，或**其全部子词同时命中**。所以 `parseHTTPResponse` 能找到 `parse_http_response`，
而 `zzqqxx_not_exist_token` 不会仅因为文件里出现 `token` 就返回一堆无关代码——它返回零结果。
多词查询里覆盖词数越多的结果排得越前。

**跨命名风格。** `find_symbol` 会把标识符归一为「去分隔符的小写子词串」再比较，
`aria2StatusStr` 因此能找到 `aria2_status_str`。模型跨语言时经常记错命名风格
（Dart camelCase / Rust snake_case），查定义的首选工具不能对此敏感。

## 验证接入

`langbot_probe.py` 用 **LangBot 同款的 MCP Python SDK** 走一遍完整流程
（initialize → tools/list → tools/call → ping），可在配置 LangBot 前先确认服务可用：

```bash
uv run --with mcp --with httpx --with anyio python langbot_probe.py
```

默认打本地。打线上用环境变量覆盖：

```bash
REPOMCP_PROBE_URL=https://你的域名/mcp REPOMCP_PROBE_TOKEN=你的token \
  uv run --with mcp --with httpx --with anyio python langbot_probe.py
```

## 局限

- 符号提取是**语言感知的正则启发式**，不是完整语法分析。选它是为了保住零依赖与 `CGO_ENABLED=0` 交叉编译；代价是复杂泛型签名、宏生成的定义可能漏抽。`search_code` 的全文检索不受此影响。
- 索引常驻内存，随仓库规模线性增长。百万行级仓库请留意进程内存。
- clone 使用 `--depth 200`，`git_history` 只能看到最近 200 次提交。
