# repoMcp

给 **LangBot** 用的仓库源码检索 MCP 服务：让 IM 机器人里的 LLM 能查你的源码、带着可核验的出处回答问题，并在调研清楚之后代用户提交与管理 issue。

- 传输：**无状态 Streamable HTTP**，单端点 `POST /mcp`，Bearer 鉴权。
- 检索：本地 clone + 内存倒排索引（BM25）+ 正则符号表。**不需要 embedding、向量库或任何外部服务**。
- issue：可选开启，走 GitHub REST。只实现「建 / 评论 / 改状态与标签」，**没有任何删除动作**，且创建前强制查重与限频。
- 依赖：**零第三方 Go 依赖**，`CGO_ENABLED=0` 单二进制。运行期只要求宿主有 `git`。

## 设计取舍

消费方是 IM 里的**小模型**：上下文窄、通常只肯调 1–3 次工具。这与 coding agent 完全不同，因此：

| 决定 | 原因 |
|---|---|
| 一次调用给足证据，不指望多轮迭代 | 小模型不会像 coding agent 那样反复 grep 收敛 |
| 输出是紧凑纯文本，不是 JSON | JSON 包装只增加 token，对模型阅读无收益 |
| 每条结果带 `路径:行号` + 钉住 commit 的 permalink | 答案必须能被人工核验，否则代码问答没有价值 |
| 硬字节预算（`maxResponseBytes`） | LangBot **不截断** tool 返回，服务端必须自己收口 |
| 工具按能力动态挂载 | 工具越多小模型选错概率越高；没接入 issue 的部署仍然只有 5 个检索工具 |
| 写操作的护栏落在服务端 | 「先调研再提 issue」「别重复提」「别随手关」写进提示词只是建议，只有服务端硬校验拦得住 |
| 词法 + 符号表，不做向量检索 | 代码检索里标识符精确匹配的召回远超 embedding；向量的复杂度换不来相应收益 |

## 工具

**检索（始终可用）**

| 工具 | 用途 |
|---|---|
| `repo_overview` | 技术栈、规模、目录结构、README 摘要。**让模型先建立坐标再检索** |
| `search_code` | 关键词检索，返回带行号与链接的片段。支持 `repo` / `lang` / `path_glob` 过滤 |
| `read_file` | 按行范围读原文（单次上限 400 行）。`blame: true` 可为每行附加最后修改的提交与作者 |
| `find_symbol` | 按名字查定义，返回签名与文档注释。已知符号名时比 `search_code` 精确 |
| `git_history` | 提交历史，回答「为什么这么写」「什么时候加的」 |

**issue / PR 查询（无条件挂载，任意公开仓库只读）**

| 工具 | 用途 |
|---|---|
| `search_issues` | 查已有 issue：回答「有人提过吗 / 现在有哪些待办」，也是创建前的查重手段 |
| `read_issue` | 读单条 issue 的完整正文与最近讨论 |
| `list_releases` | 查询 GitHub Releases 发布记录 |
| `search_pulls` | 列出 PR（状态筛选），回答「有哪些 PR / 有人提 PR 吗」 |
| `read_pull` | 读单个 PR 的完整描述、状态与分支信息 |
| `list_pull_comments` | 列 PR 的讨论评论 |

`repo` 参数支持配置短名或任意公开仓库 `owner/name`（如 `example-owner/AstrBot`）。

**issue 写入（配置了 `repos[].issues.write` 才挂载）**

| 工具 | 用途 | 权限 |
|---|---|---|
| `create_issue` | 代用户提交 issue，正文由服务端按模板渲染 | 仅配置的可写仓库；管理员（`adminReporters`）可对任意仓库（token 可访问的）写入 |
| `update_issue` | 追加评论、关闭、重开、增删标签 | 同上；**追加评论（action=comment）仅管理员可执行** |

服务在 `initialize` 时会下发 `instructions`，向模型声明可用仓库、各仓的 issue 能力、工具选择规则，以及**必须引用来源、检索无果时不得编造**。

## 配置

复制 `config.example.json` 为 `config.json`：

```json
{
  "listen": ":8790",
  "token": "长随机串",
  "dataDir": "./data",
  "syncInterval": "15m",
  "maxResponseBytes": 12000,
  "githubToken": "有 issues 权限的 PAT",
  "maxIssueCreatesPerHour": 5,
  "adminReporters": ["管理员甲", "100000001"],
  "astrbotAdminsFile": "/AstrBot/data/cmd_config.json",
  "repos": [
    {
      "name": "example-source",
      "desc": "多协议下载器主仓（这句话会展示给模型，帮它选对仓库）",
      "url": "https://github.com/upstream-owner/ExampleSource.git",
      "ref": "main",
      "webBase": "https://github.com/upstream-owner/ExampleSource",
      "exclude": ["packages/**"],
      "issues": { "write": true }
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
| `githubToken` | issue 工具用的 PAT（写操作需 `issues:write`）。留空则 issue 只能读公开仓且限流严格 |
| `githubApiBase` | API 根地址，默认 `https://api.github.com`；GHE 填 `https://<host>/api/v3` |
| `githubTimeout` | 单次 GitHub API 调用超时，默认 `20s` |
| `maxIssueCreatesPerHour` | 单仓每小时创建 issue 的上限，默认 5，`0` 表示不限 |
| `repos[].issues` | 省略 = 该仓无 issue 能力；`{}` = 只读；`{"write": true}` = 可创建与管理 |
| `repos[].issues.slug` | `owner/repo`，留空则从 `webBase` / `url` 推导；推导不出会**启动失败** |
| `repos[].issues.token` | 覆盖全局 `githubToken`（跨组织多 PAT 时用） |
| `repos[].issues.labels` | 允许模型使用的标签白名单。留空则以仓库现有标签为准 |

环境变量可覆盖：`REPOMCP_CONFIG` / `REPOMCP_LISTEN` / `REPOMCP_TOKEN` / `REPOMCP_DATA` / `REPOMCP_GITHUB_TOKEN`。

私有仓：在 `url` 中内嵌 token（如 `https://x-access-token:<PAT>@github.com/owner/repo.git`），或在宿主上预先配好凭据助手。服务已强制 `GIT_TERMINAL_PROMPT=0`，凭据缺失会直接失败而不是挂起等待输入。

## 运行

```bash
cd repoMcp
go run . -config config.json      # 本机
task build                        # 交叉编译 linux-amd64 到 build/repomcp
```

启动后服务立即可用，首次 clone 与索引在后台进行；未就绪时工具会明确回复「索引进行中」而非空结果。

探活：`GET /healthz`（无需鉴权），返回每个仓的 HEAD、文件数、符号数、上次同步时间、错误，以及 issue 能力（`off` / `read` / `write`）。全部仓库就绪前返回 `503`。

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
- 配置完成后在聊天里发 `!func` 可确认工具已注册：检索 5 个，接入 issue 后另加 2（只读）或 4 个。
- 最后把这个 MCP server 绑定到目标流水线，模型才会看到这些工具。

排障顺序：先 `curl <地址>/healthz` 看 `ready` 与每仓 `error`（此接口不需要鉴权），
再用下面的探针验证 MCP 握手，最后才怀疑 LangBot 配置。

**安全**：源码侧完全只读——不执行仓库中的任何代码，也不接受任意路径读取（`read_file` 只能读已索引的受版本控制文件，并拒绝 `..` 与绝对路径）。唯一的写入面是 issue，且实现上只有「建 issue / 评论 / 改状态与标签」四种动作，没有任何删除端点，最坏后果是多一条可被人工撤销的 issue。但本服务会把私有仓源码送进 LLM——请确认所用模型的数据策略，并把服务绑定在内网或 `127.0.0.1`。

## issue 能力

只有配了 `repos[].issues` 的仓库才会出现 issue 工具，`write` 决定挂不挂 `create_issue` / `update_issue`。

**为什么护栏在服务端**：消费方是 IM 里的小模型，「先调研再提」「别重复提」「别随手关」写进工具描述只是建议。凡是能硬校验的都在服务端拦：

| 护栏 | 行为 |
|---|---|
| 强制查重 | 创建前服务端自己再查一遍，命中疑似重复直接拒绝并列出候选；模型只能在逐条核对后带 `confirm_not_duplicate=true` 重试 |
| 双路召回 | 搜索接口覆盖历史 issue，最近 open 列表兜底中文标题（GitHub 搜索对 CJK 分词很差）。标题按重叠系数打分，阈值 0.55 |
| 频率上限 | 每仓每小时最多 `maxIssueCreatesPerHour` 个。配额在调用 GitHub **之前**扣除，失败重试也照扣 |
| 调研结论 | `confidence` 只能是 `confirmed` / `unconfirmed`；源码仓库必填，反馈仓库（无源码）可省略（默认 unconfirmed）；`confirmed` 时 `evidence` 里没有 `路径:行号` 形式的出处直接拒绝 |
| 正文服务端渲染 | 模型只能填各段内容，结构固定：问题描述 → 复现 / 触发条件（可省略）→ 环境（必填）→ 调研结论（仅源码仓库）→ 提交来源 |
| 标签白名单 | GitHub 打标签会顺手新建不存在的标签；只有仓库现有（或配置白名单里的）标签会被采用，其余忽略并在结果里说明 |
| 状态变更要理由 | `close` 必须同时给 `comment`（≥10 字结论）与 `reason`（`completed` / `not_planned`）；重复关闭已关闭的 issue 会被拒绝 |
| 仓库必须对得上 | 有多个可写仓时 `repo` 不可省略；对未接入 issue 的仓库调用会明确说明原因，而不是退而求其次挑一个 |

机器人提交的 issue 与评论都带来源标注（提交人 + `repoMcp` + 索引 commit），维护者可一眼分辨并追溯。

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
