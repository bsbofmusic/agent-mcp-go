# AMG · Agent MCP Go

[English](README.md) | **简体中文**

**让任意 AI Agent 通过标准 MCP 接管一台满血 Linux 工作空间。** 单个静态编译的 Go 二进制文件，即可把主机变成带完整鉴权的远程执行底座，覆盖文件操作、交互式 Shell、Git 协作、分块文件传输与动态 Skill/MCP 统综调度。

**公版标准 · 统综调度 · 满血执行**

![AMG 架构：兼容 MCP 的 Agent 接入独立执行核心，通过开放契约按需挂载 Skills、外部 MCP 路由与工作区记忆](assets/amg-open-architecture.svg)

[快速开始](#快速开始) · [核心架构](#核心理念公版统综满血) · [40 个原生工具](#满血工具矩阵40-个原生工具) · [开放生态](#开放扩展与推荐生态) · [配置说明](docs/configuration.md) · [安全模型](docs/security.md) · [v1.2.3 发行说明](docs/releases/v1.2.3.md)

> 本页为英文 [README](README.md) 的简体中文版。配置字段与协议细节以对应原始文档为准；修改英文 README 时同步更新本页。

## 核心理念：公版 · 统综 · 满血

大多数远程 Agent 方案要么用脆弱的胶水脚本硬套自定义协议，要么为改几个文件就强迫部署一整套重型微服务栈。AMG 走的是另一条路：

1. **公版底座，零重复造轮子**
   - 直接基于官方 Go MCP SDK（`github.com/modelcontextprotocol/go-sdk`）、标准 Streamable HTTP 传输协议（有状态 `/mcp` 与无状态 `/mcp/stateless`）以及标准 OAuth 2.1 + PKCE（`S256`）+ RFC 7591 动态客户端注册（DCR）+ RFC 9728/8414 自动发现构建，不魔改协议。
   - 编译为单个 `CGO_ENABLED=0` 静态二进制，全库仅 5 个直接 Go 依赖（`go-sdk`、`jsonschema-go`、`doublestar/v4`、`yaml.v3`、`x/time/rate`），底层直接调用系统 POSIX Shell（`os/exec`）与原生 `git`。
   - 对外只暴露干净的文件与 CLI 开放契约（`skills_root`、`shared_mcp_bin`、磁盘归档），不捆绑外部组件。单二进制即可独立运行，也能随时接入你现有的工具链。

2. **统综调度，单一真源不分裂**
   - **一个工作空间，多端通用：** 同时兼容 ChatGPT、Gemini 网页版、Claude、豆包工作（Doubao Work）、Qoder 以及各类支持 Streamable HTTP 的桌面端或 CLI Agent，不绑定任何模型厂商。
   - **单一真源，动态按需加载：** AMG 绝不镜像外部目录，也不养后台同步守护进程。握手初始化（`initialize`）时只下发有界紧凑的 Skills 索引（由 `skill_index_entry_bytes` 与 `skill_index_max_bytes` 控制预算）以及工作区 `AGENTS.md`、可选 `bootstrap_files`；只有模型真正需要时才按需读取 Skill 全文（`skills_read`）或实时查询外部 MCP 路由与 Schema（`mcp_list_sources`、`mcp_search_tools`、`mcp_get_tool`、`mcp_call_tool`）。Skill 正文或上游路由一改，下一次调用立刻生效，无需重启服务。
   - **多 Agent 并发与隔离：** 支持多个 Agent 并发连接同一台 AMG 实例。按 `scope` 划分的互斥锁隔离各自的任务计划、笔记和跨连接交接状态；按仓库加锁并结合按 scope 隔离的 Git 引用（`refs/amg/review/*`）隔离变更审查；配合 `git worktree` 可让多个并发 Agent 在同一仓库并行改代码而不互相覆盖。

3. **满血执行，生产级稳态加固**
   - **开箱即用 40 个原生工具：** 覆盖目录浏览、正则搜索、行号窗口读取、带原子回滚的 Codex 风格 `apply_patch`、图片查看、完整 Git 工作流（`status`、`log`、`commit`、`push`）、支持超时挂起与交互输入的后台 Shell 会话（`exec_command` + `write_stdin`，采用 46 位浮点安全句柄）、300 MiB+ 分块文件传输（`transfer_prepare`，16 MiB PATCH 分片、SHA-256 校验与默认防覆盖保护）以及 MCP 变更审查界面（`show_changes`）。
   - **网页端零门槛丝滑授权：** 内置 OAuth 2.1 授权服务器，默认签发 30 天 Access Token 与不轮换的 90 天 Refresh Token，完整支持浏览器 CORS 预检，并提供 `annotations_mode` 切换（标准风险语义 `default` 与私有单人部署免频繁弹窗的 `trusted` 模式）。Gemini 网页版等托管客户端无需预先填写 Client ID/Secret 即可零配置动态注册并流畅执行。
   - **高负载下依然快、稳、有界：** `os.OpenRoot` 目录沙箱阻断符号链接越界；Skills 缓存通过 `(inode, size, mtime, ctime)` stat 快路径实现命中零文件打开；变更审查基线内置超时与大仓库跳过白名单（`review_baseline_timeout_seconds` 与 `review_baseline_skip_paths`），遇到数 GB 超大仓库时自动降级为 `HEAD^{tree}`，绝不拖死进程；18 项运行时参数全部支持 `settings_update` 热更新，无需重启。

## 满血工具矩阵（40 个原生工具）

| 能力域 | 包含工具 | 核心特性 |
| --- | --- | --- |
| **文件系统与补丁** | `list_directory`、`tree`、`glob`、`grep`、`read_file`、`write_file`、`apply_patch`、`view_image`、`get_project_doc` | `os.OpenRoot` 路径越界防护、遵循 `.gitignore` 的检索、支持失败回滚的多文件原子补丁、有界图片读取、层级化 `AGENTS.md` 加载 |
| **Shell 与进程控制** | `exec_command`、`write_stdin`、`stop_command`、`run_command`、`clock_curr_time`、`clock_sleep` | POSIX Shell 执行、动态可调等待窗口、后台长任务续接、Ctrl-C 与进程组终止、无 Shell 解析的直连二进制执行 |
| **Git 与变更审查** | `git_status`、`git_log`、`git_commit`、`git_push`、`show_changes` | 按仓库串行化基线快照、按 `scope` 隔离的 `refs/amg/review/*` 追踪、有界快照超时保护、MCP Review UI 资源 |
| **文件传输与附件** | `transfer_prepare`、`export_host_file`、`import_host_file`、`import_chatgpt_file` | 限时单用途 HTTP 上传/下载链接，支持 `max_file_bytes`（默认 300 MiB）、16 MiB PATCH 分片、SHA-256 校验与默认防覆盖（需显式 `overwrite=true` 才覆盖） |
| **Skills 与共享 MCP** | `skills_list`、`skills_read`、`mcp_list_sources`、`mcp_search_tools`、`mcp_get_tool`、`mcp_call_tool`（及 3 个 `shared_mcp_*` 兼容别名） | stat 快路径缓存的 Skill 目录发现、按需读取附属文档、实时动态发现上游 MCP 模块并原样透传原生返回结构 |
| **任务连续性与热调参** | `get_agent_brief`、`update_plan`、`remember`、`recall`、`get_environment`、`settings_get`、`settings_update` | 按 `scope` 隔离的计划与任务笔记、匿名会话 30 天自动清理、18 项运行参数不停机热更新 |

## 开放扩展与推荐生态

AMG 核心完全独立运行。当你需要共享 Skill 库、外部 MCP 服务或跨会话长期记忆时，按以下开放契约接入任意兼容实现即可：

| 能力 | 开放接入契约 | 推荐方案（仅作示例） |
| --- | --- | --- |
| **Skills** | [`skills_root`](docs/integrations.md#local-skills)：存放 `SKILL.md` 技能包的本地目录 | Skills Manager |
| **外部 MCP 工具** | [`shared_mcp_bin`](docs/integrations.md#shared-mcp-cli-contract)：兼容的 stdio 路由适配器可执行文件 | [AgentGateway](https://github.com/agentgateway/agentgateway) + 兼容适配器 |
| **持久化记忆** | [本地磁盘归档](docs/integrations.md#task-continuity-and-memory)：位于 `work_root` 内的 Markdown 与会话归档 | [Obsidian](https://obsidian.md/) + [AgentsView](https://github.com/kenn-io/agentsview) |

AMG 不负责安装、捆绑或托管这些外部组件。将 `skills_root` 或 `shared_mcp_bin` 留空，即可保持单二进制纯净运行。

## 快速开始

**环境要求：** Linux amd64、`curl`、GNU `sha256sum`、`tar` 和 Python 3（服务器需具备 POSIX Shell 与 `git`）。请使用专用非 root 系统账户运行 AMG；持有有效令牌即代表拥有该系统账户的执行权限（详见[安全模型](docs/security.md)）。

### 1. 下载并校验

```sh
mkdir amg-v1.2.5
cd amg-v1.2.5
release=https://github.com/bsbofmusic/agent-mcp-go/releases/download/v1.2.5
curl -fLO "$release/agent-mcp-go-v1.2.5-linux-amd64.tar.gz"
curl -fLO "$release/checksums.txt"
sha256sum --ignore-missing -c checksums.txt
tar -xzf agent-mcp-go-v1.2.5-linux-amd64.tar.gz
install -Dm755 agent-mcp-go "$HOME/.local/bin/agent-mcp-go"
```

### 2. 生成个人配置

发行包中的 [config.example.json](config.example.json) 使用 `/home/amg` 占位路径。以下脚本会自动替换为当前用户的真实绝对路径，创建 `0700` 权限的工作空间、状态与 Skills 目录，并生成 `0600` 权限的随机 Owner Token（若配置或令牌已存在则拒绝覆盖）：

```sh
python3 - <<'PY'
import json, os, secrets
from pathlib import Path

os.umask(0o077)
home = Path.home().resolve()
config_dir = home / ".config/agent-mcp-go"
config_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
config_path = config_dir / "config.json"
token_path = config_dir / "mcp-token"
if config_path.exists() or token_path.exists():
    raise SystemExit("Existing config/token found; review them instead of overwriting.")
config = json.loads(Path("config.example.json").read_text())
config.update({
    "listen": "127.0.0.1:3100",
    "work_root": str(home / "workspace"),
    "default_workdir": str(home / "workspace"),
    "state_dir": str(home / ".local/state/agent-mcp-go"),
    "skills_root": str(home / ".config/agent-mcp-go/skills"),
    "token_file": str(token_path),
    "public_url": "http://127.0.0.1:3100",
    "allowed_origins": [],
    "shared_mcp_bin": "",
    "executable_dirs": [],
})
for key in ("work_root", "state_dir", "skills_root"):
    Path(config[key]).mkdir(parents=True, exist_ok=True, mode=0o700)
with token_path.open("x") as f:
    f.write(secrets.token_hex(32) + "\n")
token_path.chmod(0o600)
with config_path.open("x") as f:
    json.dump(config, f, indent=2)
    f.write("\n")
config_path.chmod(0o600)
print("Configuration:", config_path)
PY
```

AMG 在 JSON 配置中只接受字面绝对路径，不会自动展开 `~` 或 `$HOME`。

### 3. 启动并检查健康状态

```sh
"$HOME/.local/bin/agent-mcp-go" --config "$HOME/.config/agent-mcp-go/config.json"
```

在另一个终端验证：

```sh
curl -fsS http://127.0.0.1:3100/healthz
```

如需作为后台服务长期运行并启用 cgroup 内存护栏（`MemoryHigh=400M`、`MemoryMax=800M`、`MemorySwapMax=512M`），参考 [systemd/agent-mcp-go.service.example](systemd/agent-mcp-go.service.example)。

### 4. 连接客户端

**静态 Bearer 请求头（适用于 CLI、桌面端或本地客户端）：**

| 配置项 | 填写值 |
| --- | --- |
| **URL** | `http://127.0.0.1:3100/mcp`（若客户端不保留 `Mcp-Session-Id`，改用 `/mcp/stateless`） |
| **Header** | `Authorization: Bearer <mcp-token 文件内容>` |

**托管网页端 OAuth 2.1 授权（适用于 Google Gemini 网页版、Claude 网页版、ChatGPT）：**

1. 通过 HTTPS 反向代理或隧道将 `127.0.0.1:3100` 暴露到公网，在 `config.json` 中将 `public_url` 设为对应的 HTTPS 源站地址，把客户端源站加入 `allowed_origins`，并设置 `"oauth": { "enabled": true }`。
2. 在客户端的自定义 MCP 连接器中填入 `<public_url>/mcp/stateless`（Gemini 网页版强烈推荐）或 `<public_url>/mcp`，`客户端 ID` 与 `客户端密钥` 留空即可自动走零配置动态注册（DCR）。
3. 在弹出的授权确认页输入 `mcp-token` 文件内容完成放行。默认 Access Token 有效期 30 天，Refresh Token 有效期 90 天。

**Gemini 网页版接入关键注意事项（实测避坑清单）：**

- **优先填写 `/mcp/stateless`，不要填 `/mcp`：** Gemini 网页版在多轮对话或新开对话之间会重连，且不会稳定保留 `Mcp-Session-Id`，容易导致后续轮次静默丢失工具绑定。改用 `<public_url>/mcp/stateless` 可彻底消除会话状态漂移，同时完整保留 OAuth 2.1、`trusted` 免确认模式以及 46 位浮点安全的后台 Shell 续接句柄。
- **务必使用独立子域名（如 `https://amg.<你的域名>`），不要挂在子路径下：** OAuth 2.1 自动发现端点（`/.well-known/*`）与授权端点（`/oauth/*`）必须位于源站根路径（`/`）。挂在子路径（如 `/amg`）下会破坏标准 RFC 9728/8414 发现与浏览器 `Origin` 校验；独立子域名还能将 TLS 证书、CORS 策略和超时配置与其他站点彻底隔离。
- **反向代理与 CDN 配置：** 为子域名配置可信公网 TLS，关闭响应缓冲（response buffering），将代理读取超时调高至 600s–900s 以适配长命令与大文件传输，并原样透传 `Host`、`Authorization`、`Accept`、`Origin`、`Sec-Fetch-Site` 与 `Mcp-Session-Id` 请求头。若使用 Cloudflare，建议开启纯 DNS 灰云直连，或确保 WAF / 机器人防护不会拦截 `/.well-known/*`、`/oauth/register` 与 `/mcp/stateless`。
- **授权密码、`trusted` 模式与显式 `scope`：** 确保浏览器未拦截 `gemini.google.com` 的授权弹窗；在 AMG 授权确认页输入本机 `mcp-token` 文件内容（纯 `<token>` 或 `Bearer <token>` 均可），而不是 Google 账号密码。私有单人部署建议在 `config.json` 中启用 `"annotations_mode": "trusted"`，免去每次工具调用的重复确认弹窗。在 Gemini 对话中使用时，需从 `@` 弹出菜单点选应用胶囊标签（纯手打文字无效）；在无状态模式下跨轮共享 `update_plan`、`remember`、`recall` 或 `show_changes` 状态时，显式传入同一个 `scope` 参数即可。

详细配置与常见客户端授权说明见 [Gemini Web 与 OAuth 客户端](docs/integrations.md#gemini-web-and-oauth-clients) 以及[重复工具确认排障说明](docs/integrations.md#gemini-repeated-tool-confirmations)。

## 多 Agent 并发协作

- **按 `scope` 隔离任务状态：** 为每个并发 Agent 传入独立的 `scope` 字符串，即可隔离各自的 `update_plan`、`remember`、`recall` 与 `show_changes` 状态；未指定 `scope` 的匿名会话状态保留 30 天后自动清理。
- **并行修改同一仓库：** 为每个同时改代码的 Agent 分配独立的 `git worktree` 目录，并将该路径传入 `workdir`（`exec_command` / `run_command`）或 `directory`（Git 工具）。
- **单写入者原则：** 后台共享服务启停与 `settings_update` 参数调整保持由单一协调者执行。

## 从源码构建与测试

需要 **Go 1.25+**：

```sh
CGO_ENABLED=0 go test -count=1 ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go build -buildvcs=false -trimpath -o agent-mcp-go .
```

需要真实配置环境的集成测试默认关闭，可通过设置 `AGENT_MCP_GO_CONFIG` 指向临时测试工作区启用（详见 [v1.2.3 发行说明](docs/releases/v1.2.3.md#installation-and-verification)）。

## 文档导航

- [配置参考](docs/configuration.md)：全部配置字段、OAuth 2.1 参数、资源上限与 systemd 单元示例。
- [客户端与生态接入](docs/integrations.md)：Gemini 网页版、Claude 网页版、ChatGPT、本地 Skills、共享 MCP CLI 契约与任务连续性。
- [安全模型](docs/security.md)：系统账户隔离、令牌管理、临时传输链接与并发限制。
- [变更日志](CHANGELOG.md) 与 [v1.2.3 发行说明](docs/releases/v1.2.3.md)。

## 许可证与致谢

本项目基于 [MIT 许可证](LICENSE) 开源。工作空间工具语义与文档结构参考了 [Codex Free](https://github.com/hypnguyen1209/codex-free)。内置代码审查界面改编自 Codex Free v1.9.1（MIT 许可，版权所有 © 2026 hypnguyen1209），并包含 [Prism](https://prismjs.com/)（MIT 许可，版权所有 © 2012 Lea Verou），相关声明保留在 [assets/CODEXFREE-REVIEW-LICENSE.txt](assets/CODEXFREE-REVIEW-LICENSE.txt)。
