# ACPNet

Bridge ACP adapters running on a macOS host into OrbStack, Docker, or any other remote environment over raw TCP or HTTP CONNECT.

`acpnet` was built for one specific pain point:

- `acpx` and OpenClaw can run inside a container
- `codex` and `claude code` often live on the macOS host
- ACP adapters expect to be spawned locally over stdio

This project turns that local stdio boundary into a network hop while keeping the ACP stream intact.

## What it does

- Runs a **host-side bridge server** that starts ACP adapters such as:
  - `@zed-industries/codex-acp`
  - `@zed-industries/claude-agent-acp`
- Runs a **client-side shim** inside a container or remote machine
- Forwards ACP stdio traffic over:
  - raw TCP
  - HTTP CONNECT
- Optionally rewrites absolute paths in ACP NDJSON messages so container paths and host paths can differ

## Why this exists

`acpx` is designed to spawn ACP adapters locally. In a containerized setup, that means:

- the container can run `acpx`
- the host can run `codex` / `claude`
- but the two cannot talk over local stdio directly

`acpnet` fills that gap without patching `acpx`, `codex-acp`, `claude-agent-acp`, or OpenClaw.

## Verified scenarios

These were manually tested on **March 15, 2026** on macOS + OrbStack:

| Scenario | Status |
| --- | --- |
| Local raw TCP bridge with generic stdio process (`cat`) | Verified |
| Container `acpx codex` -> host Codex over raw TCP | Verified |
| Container `acpx claude` -> host Claude Code over raw TCP | Verified |
| Container path `/workspace/...` -> host path `/Users/...` with `--map`, Codex | Verified |
| Container path `/workspace/...` -> host path `/Users/...` with `--map`, Claude Code | Verified |
| Container `acpx codex` -> host Codex over HTTP CONNECT | Verified |
| Container `acpx claude` -> host Claude Code over HTTP CONNECT | Verified |

## Architecture

```text
container / remote env
  acpx / OpenClaw / any ACP client
          |
          | spawn
          v
  acpnet client
          |
          | TCP or HTTP CONNECT
          v
macOS host
  acpnet serve
          |
          | spawn
          v
  codex-acp / claude-agent-acp
          |
          v
  codex / claude code
```

## Protocol model

The bridge uses a small handshake, then tunnels the remaining ACP traffic.

1. Client connects over raw TCP or HTTP CONNECT
2. Client sends one JSON line:

```json
{"token":"...","agent":"codex","cwd":"/workspace/project"}
```

3. Server validates the token, resolves the target adapter, maps the working directory if needed, and starts the host-side adapter
4. Server responds with one JSON line:

```json
{"ok":true}
```

5. The rest of the stream is forwarded bidirectionally

When path mappings are configured, the bridge rewrites absolute paths inside JSON lines in both directions. This is what makes `/workspace/...` inside the container work against `/Users/...` on the host.

## Install

### Build from source

```bash
git clone https://github.com/your-org/acpnet.git
cd acpnet

go build -o dist/acpnet-darwin-arm64 .
GOOS=linux GOARCH=arm64 go build -o dist/acpnet-linux-arm64 .
```

### Homebrew

Homebrew support is included via GoReleaser. After the project is published:

```bash
brew install your-tap/acpnet
```

## Usage

### 1. Start the host bridge

Raw TCP only:

```bash
TOKEN='replace-with-a-random-secret'

./dist/acpnet-darwin-arm64 serve \
  --listen 0.0.0.0:4601 \
  --token "$TOKEN"
```

Raw TCP + HTTP CONNECT:

```bash
TOKEN='replace-with-a-random-secret'

./dist/acpnet-darwin-arm64 serve \
  --listen 0.0.0.0:4601 \
  --http-listen 0.0.0.0:4603 \
  --http-path /v1/connect \
  --token "$TOKEN"
```

With path mapping:

```bash
./dist/acpnet-darwin-arm64 serve \
  --listen 0.0.0.0:4601 \
  --http-listen 0.0.0.0:4603 \
  --token "$TOKEN" \
  --map /workspace=/Users/zhangwei/work
```

### 2. Use the client from a container

Raw TCP:

```bash
/workspace/acpnet/dist/acpnet-linux-arm64 \
  client \
  --server tcp://host.docker.internal:4601 \
  --token "$TOKEN" \
  --agent codex
```

HTTP CONNECT:

```bash
/workspace/acpnet/dist/acpnet-linux-arm64 \
  client \
  --server http://host.docker.internal:4603/v1/connect \
  --token "$TOKEN" \
  --agent codex
```

If `--server` does not include a scheme, it defaults to raw TCP.

## Using with acpx

The cleanest setup is to override `acpx` agent aliases in `~/.acpx/config.json`.

### Raw TCP example

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

### HTTP CONNECT example

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

Then inside the container:

```bash
acpx codex exec "Reply with exactly OK."
acpx claude exec "Reply with exactly OK."
```

## Using with OpenClaw

This bridge is designed to work well with containerized OpenClaw setups that delegate through `acpx`.

Recommended pattern:

1. Run OpenClaw inside the container
2. Install and enable the OpenClaw `acpx` plugin
3. Configure container-local `~/.acpx/config.json` to point `codex` / `claude` to `acpnet client`
4. Run `acpnet serve` on the host

This avoids patching OpenClaw source code.

## Defaults

If you do not override adapter commands, the host server uses:

- `codex`: `npx -y @zed-industries/codex-acp@0.10.0`
- `claude`: `npx -y @zed-industries/claude-agent-acp@0.21.0`

Override them if you need different versions:

```bash
./dist/acpnet-darwin-arm64 serve \
  --token "$TOKEN" \
  --codex-cmd 'npx -y @zed-industries/codex-acp@0.10.0' \
  --claude-cmd 'npx -y @zed-industries/claude-agent-acp@0.21.0'
```

## Health check

When HTTP mode is enabled:

```bash
curl http://127.0.0.1:4603/healthz
```

Example response:

```json
{"ok":true,"path":"/v1/connect","version":"dev"}
```

## CLI reference

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

## Development

### Run tests

```bash
go test ./...
```

### What is covered by CI

- JSON path rewrite logic
- raw TCP bridge round trip
- HTTP CONNECT bridge round trip

### What still needs manual smoke testing

Real Codex / Claude Code integrations require local credentials and are not suitable for public CI.

## Security notes

- The bridge is not anonymous. Use a strong random token.
- Raw TCP should usually be bound to a private interface.
- HTTP CONNECT mode is convenient for remote routing, but you should still put it behind your own network boundary or reverse proxy.
- Path rewriting is intentionally simple: it rewrites absolute path prefixes in JSON values. That is sufficient for ACP NDJSON traffic, but you should still test your exact workflow before exposing it broadly.

## Roadmap

- TLS termination and reverse-proxy deployment examples
- Optional allowlists for source IPs or agents
- Better metrics and structured logs
- Packaged examples for OpenClaw + OrbStack

## License

MIT
