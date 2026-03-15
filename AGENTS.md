# Repository Guidelines

## Project Structure & Module Organization

`acpnet` is a small Go CLI with a flat layout. `main.go` contains the bridge server, client, transport logic, and CLI entrypoints. `main_test.go` covers core protocol and rewrite behavior. `testdata/` stores prompt files and `acpx` config fixtures used during manual and automated verification. `scripts/verify-brew-e2e.sh` validates the published Homebrew build and optional container flow. Release automation lives in `.github/workflows/`, `.goreleaser.yml`, and `.goreleaser.beta.yml`. `dist/` is generated output and should not be edited manually.

## Build, Test, and Development Commands

- `go test ./...`: run the Go unit tests.
- `go build .`: build the local development binary.
- `goreleaser check`: validate the stable release config.
- `goreleaser check --config .goreleaser.beta.yml`: validate the beta release config.
- `./scripts/verify-brew-e2e.sh`: verify the installed Homebrew binary against local `acpx`, `codex`, and `claude`.
- `ACPNET_E2E_IMAGE=agent0ai/agent-zero:latest ./scripts/verify-brew-e2e.sh --container`: run the full local + container end-to-end verification.

## Coding Style & Naming Conventions

Follow standard Go style and keep the code `gofmt`-clean. Use tabs as Go expects, short helper names only when the scope is obvious, and descriptive flag or config names for user-facing behavior. Keep shell scripts POSIX-friendly where practical, but this repository currently uses Bash with `set -euo pipefail`. Prefer explicit, readable flag names such as `--http-listen` and `--codex-cmd`.

## Testing Guidelines

Add unit tests to `main_test.go` for protocol changes, path rewriting, and transport behavior. Keep fixtures in `testdata/` and name them by scenario, for example `acpx-config.http-codex.json`. Before tagging a release, run `go test ./...` and the published-binary verification script. Changes that affect release packaging should also be checked with `goreleaser check`.

## Commit & Pull Request Guidelines

Use short Conventional Commit prefixes seen in the history: `feat:`, `fix:`, `docs:`, `ci:`, `chore:`. Keep the subject imperative and specific, for example `docs: add brew verification guide and script`. PRs should describe the user-visible change, note any release or Homebrew impact, and include exact verification commands and results. Link issues when relevant.

## Release & Configuration Notes

Stable tags use `vX.Y.Z` and publish GitHub releases plus Homebrew updates. Beta tags use `vX.Y.Z-beta.N` and publish prereleases only. Do not hardcode secrets in scripts or docs; use repository secrets and variables referenced by the GitHub workflows.
