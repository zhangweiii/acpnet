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

## 安装

### 从源码构建

```bash
git clone https://github.com/your-org/acpnet.git
cd acpnet

go build -o dist/acpnet-darwin-arm64 .
GOOS=linux GOARCH=arm64 go build -o dist/acpnet-linux-arm64 .
```

### Homebrew

项目已经准备好通过 GoReleaser 发布到 Homebrew。正式发布后可以这样安装：

```bash
brew install your-tap/acpnet
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
