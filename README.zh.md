# ACPNet

把运行在 macOS 宿主机上的 ACP adapter，通过原始 TCP 或 HTTP CONNECT，桥接给 OrbStack、Docker 或其他远程环境里的 `acpx` / OpenClaw 使用。

这个项目解决的是一个很具体的问题：

- `acpx` 或 OpenClaw 跑在容器里
- `codex` 和 `claude code` 跑在 macOS 宿主机
- ACP adapter 默认要求“本地 stdio 直连”

`acpnet` 把这个“本地 stdio 边界”变成一次网络跳转，同时尽量不修改上游工具。

## 能做什么

- 在宿主机启动一个 **bridge server**
- 在容器或远端环境启动一个 **bridge client**
- 在两端之间转发 ACP stdio 流量
- 支持两种传输：
  - 原始 TCP
  - HTTP CONNECT
- 支持 ACP NDJSON 中绝对路径的双向重写，让容器路径和宿主机路径可以不同

## 为什么需要它

`acpx` 的模型是“本地 spawn ACP adapter”。在容器化场景下，这意味着：

- 容器里能跑 `acpx`
- 宿主机上能跑 `codex` / `claude`
- 但两边没法直接共享本地 stdio

这个桥接项目填的就是这块空白，而且不需要去 patch `acpx`、OpenClaw、`codex-acp` 或 `claude-agent-acp`。

## 已实测场景

以下场景已在 **2026-03-15** 于 macOS + OrbStack 上真实验证：

| 场景 | 状态 |
| --- | --- |
| 本机原始 TCP + 通用 stdio 进程（`cat`） | 已验证 |
| 容器内 `acpx codex` -> 宿主机 Codex（raw TCP） | 已验证 |
| 容器内 `acpx claude` -> 宿主机 Claude Code（raw TCP） | 已验证 |
| 容器路径 `/workspace/...` -> 宿主机 `/Users/...`，Codex + `--map` | 已验证 |
| 容器路径 `/workspace/...` -> 宿主机 `/Users/...`，Claude Code + `--map` | 已验证 |
| 容器内 `acpx codex` -> 宿主机 Codex（HTTP CONNECT） | 已验证 |
| 容器内 `acpx claude` -> 宿主机 Claude Code（HTTP CONNECT） | 已验证 |

## 架构

```text
容器 / 远端环境
  acpx / OpenClaw / 其他 ACP client
          |
          | spawn
          v
  acpnet client
          |
          | TCP 或 HTTP CONNECT
          v
macOS 宿主机
  acpnet serve
          |
          | spawn
          v
  codex-acp / claude-agent-acp
          |
          v
  codex / claude code
```

## 协议模型

桥接层先做一次轻量握手，之后把 ACP 流量继续隧道转发。

1. client 通过 TCP 或 HTTP CONNECT 连到 server
2. client 先发送一行 JSON：

```json
{"token":"...","agent":"codex","cwd":"/workspace/project"}
```

3. server 校验 token，决定启动哪个 adapter，并在需要时对工作目录做路径映射
4. server 返回一行 JSON：

```json
{"ok":true}
```

5. 后续流量双向转发

当配置了 `--map` 时，bridge 会对 JSON 行中的绝对路径做双向重写。这也是为什么容器里的 `/workspace/...` 可以映射到宿主机的 `/Users/...`。

## `--map` 路径映射怎么用

只要客户端这一侧的路径和宿主机上的实际路径不一致，就应该配置 `--map`。

例如：

- 容器 / OpenClaw 这一侧项目路径是 `/app`
- macOS 宿主机上的实际项目路径是 `/Users/zhangwei/work/my-project`

宿主机应该这样启动：

```bash
acpnet serve \
  --listen 0.0.0.0:4601 \
  --token "$TOKEN" \
  --map /app=/Users/zhangwei/work/my-project
```

`--map` 实际做的事情：

- 在宿主机启动 adapter 之前，把传入的 `cwd` 改成宿主机路径
- 对 ACP JSON 行里的绝对路径做双向重写
- 让容器继续看到 `/app/...`，而宿主机 agent 看到 `/Users/...`

什么时候必须配：

- 容器路径和宿主机路径不同
- OpenClaw 或 `acpx` 跑在 Docker / OrbStack 里
- 宿主机 agent 需要对同一份代码工作，但两边绝对路径不同

什么时候可以不配：

- 客户端和宿主机本来就是同一个绝对路径

最常见的错误：

- 客户端工作目录是 `/app`
- 宿主机没有加 `--map`
- 然后宿主机会报类似 `stat "/app": no such file or directory`

一个重要限制：

- `acpnet` 只桥接 ACP 流量，不同步文件系统
- 如果客户端跑在另一台机器上，宿主机也必须有同一份代码
- `--map` 只能翻译路径，不能帮你复制代码或挂载目录
- 如果宿主机本地没有那份项目，Codex 或 Claude Code 仍然无法正常工作

## 安装

### Homebrew

安装已发布版本：

```bash
brew install zhangweiii/tap/acpnet
```

升级到最新发布版本：

```bash
brew upgrade acpnet
```

确认当前安装版本：

```bash
acpnet version
```

### 从源码构建

```bash
git clone https://github.com/your-org/acpnet.git
cd acpnet

go build -o dist/acpnet-darwin-arm64 .
GOOS=linux GOARCH=arm64 go build -o dist/acpnet-linux-arm64 .
```

## 使用方式

### 1. 在宿主机启动 bridge server

只开原始 TCP：

```bash
TOKEN='replace-with-a-random-secret'

./dist/acpnet-darwin-arm64 serve \
  --listen 0.0.0.0:4601 \
  --token "$TOKEN"
```

同时开启 raw TCP 和 HTTP CONNECT：

```bash
TOKEN='replace-with-a-random-secret'

./dist/acpnet-darwin-arm64 serve \
  --listen 0.0.0.0:4601 \
  --http-listen 0.0.0.0:4603 \
  --http-path /v1/connect \
  --token "$TOKEN"
```

配置路径映射：

```bash
./dist/acpnet-darwin-arm64 serve \
  --listen 0.0.0.0:4601 \
  --http-listen 0.0.0.0:4603 \
  --token "$TOKEN" \
  --map /workspace=/Users/zhangwei/work
```

如果容器里使用的是 `/app` 而不是 `/workspace`，就应该改成：

```bash
./dist/acpnet-darwin-arm64 serve \
  --listen 0.0.0.0:4601 \
  --token "$TOKEN" \
  --map /app=/Users/zhangwei/work/my-project
```

### 2. 在容器里使用 bridge client

raw TCP：

```bash
/workspace/acpnet/dist/acpnet-linux-arm64 \
  client \
  --server tcp://host.docker.internal:4601 \
  --token "$TOKEN" \
  --agent codex
```

HTTP CONNECT：

```bash
/workspace/acpnet/dist/acpnet-linux-arm64 \
  client \
  --server http://host.docker.internal:4603/v1/connect \
  --token "$TOKEN" \
  --agent codex
```

如果 `--server` 不带协议头，默认按 raw TCP 处理。

## 与 acpx 一起使用

最推荐的方式是直接改容器内的 `~/.acpx/config.json`，把 `codex` / `claude` 这两个 alias 指向 bridge client。

### raw TCP 示例

```json
{
  "agents": {
    "codex": {
      "command": "/workspace/acpnet/dist/acpnet-linux-arm64 client --server tcp://host.docker.internal:4601 --token YOUR_TOKEN --agent codex"
    },
    "claude": {
      "command": "/workspace/acpnet/dist/acpnet-linux-arm64 client --server tcp://host.docker.internal:4601 --token YOUR_TOKEN --agent claude"
    }
  }
}
```

### HTTP CONNECT 示例

```json
{
  "agents": {
    "codex": {
      "command": "/workspace/acpnet/dist/acpnet-linux-arm64 client --server http://host.docker.internal:4603/v1/connect --token YOUR_TOKEN --agent codex"
    },
    "claude": {
      "command": "/workspace/acpnet/dist/acpnet-linux-arm64 client --server http://host.docker.internal:4603/v1/connect --token YOUR_TOKEN --agent claude"
    }
  }
}
```

然后容器里就可以直接用：

```bash
acpx codex exec "Reply with exactly OK."
acpx claude exec "Reply with exactly OK."
```

## 与 OpenClaw 一起使用

这个项目尤其适合“容器里跑 OpenClaw，通过 `acpx` 去调宿主机 CLI”的场景。

推荐模式：

1. OpenClaw 跑在容器里
2. 安装并启用 OpenClaw 的 `acpx` 插件
3. 在容器里配置 `~/.acpx/config.json`
4. 宿主机启动 `acpnet serve`

这样不用改 OpenClaw 源码。

## 端到端验证

仓库里附带了一份针对 Homebrew 发布版的验证脚本。

只验证本机链路：

```bash
./scripts/verify-brew-e2e.sh
```

连容器链路一起验证：

```bash
./scripts/verify-brew-e2e.sh --container
```

如果你本机已经有带 `node`、`npm`、`npx` 的可用镜像，可以覆盖默认镜像：

```bash
ACPNET_E2E_IMAGE=agent0ai/agent-zero:latest ./scripts/verify-brew-e2e.sh --container
```

脚本会验证：

- brew 安装的宿主机 `acpnet serve`
- raw TCP 和 HTTP CONNECT
- 本机 `acpx -> acpnet -> host codex`
- 本机 `acpx -> acpnet -> host claude`
- 可选的容器链路：`acpx -> release Linux acpnet client -> brew 宿主机 acpnet`

依赖要求：

- 通过 Homebrew 安装好的本机 `acpnet`
- `npx`
- `codex`
- `claude`
- 使用 `--container` 时还需要 `docker`

可覆盖环境变量：

- `ACPNET_E2E_IMAGE`: `--container` 使用的容器镜像
- `ACPNET_E2E_WORKSPACE`: 挂载为 `/workspace` 的宿主机路径
- `ACPNET_E2E_REPO_OWNER` / `ACPNET_E2E_REPO_NAME`: release 下载源覆盖

## 默认 adapter 命令

如果你不手动覆盖，宿主机 server 默认会用：

- `codex`: `npx -y @zed-industries/codex-acp@0.10.0`
- `claude`: `npx -y @zed-industries/claude-agent-acp@0.21.0`

如果你想固定成别的版本：

```bash
./dist/acpnet-darwin-arm64 serve \
  --token "$TOKEN" \
  --codex-cmd 'npx -y @zed-industries/codex-acp@0.10.0' \
  --claude-cmd 'npx -y @zed-industries/claude-agent-acp@0.21.0'
```

## 健康检查

启用 HTTP 模式后：

```bash
curl http://127.0.0.1:4603/healthz
```

返回示例：

```json
{"ok":true,"path":"/v1/connect","version":"dev"}
```

## CLI 参考

### Server

```bash
acpnet serve \
  --listen 0.0.0.0:4601 \
  --http-listen 0.0.0.0:4603 \
  --http-path /v1/connect \
  --token YOUR_TOKEN \
  --map /workspace=/Users/zhangwei/work \
  --codex-cmd 'npx -y @zed-industries/codex-acp@0.10.0' \
  --claude-cmd 'npx -y @zed-industries/claude-agent-acp@0.21.0'
```

### Client

```bash
acpnet client \
  --server tcp://host.docker.internal:4601 \
  --token YOUR_TOKEN \
  --agent codex \
  --cwd /workspace/project
```

### Version

```bash
acpnet version
```

## 开发

### 运行测试

```bash
go test ./...
```

### CI 当前覆盖的内容

- JSON 路径重写逻辑
- raw TCP round trip
- HTTP CONNECT round trip

### 仍需要手工 smoke test 的部分

真实 Codex / Claude Code 调用依赖本地登录态和凭据，不适合放进公开 CI。

## 安全说明

- 不要用弱 token。请使用高强度随机字符串。
- raw TCP 最好只监听在私有网卡或可信网络。
- HTTP CONNECT 虽然更适合远程接入，但仍建议放在内网、VPN 或反向代理之后。
- 路径重写逻辑刻意保持简单：只重写 JSON 值里的绝对路径前缀。对于 ACP NDJSON 足够实用，但正式暴露前仍建议用你的实际工作流完整回归。

## 路线图

- TLS / 反向代理部署示例
- 源地址 / agent allowlist
- 更好的 metrics 和结构化日志
- OpenClaw + OrbStack 的完整打包示例

## 许可证

MIT
