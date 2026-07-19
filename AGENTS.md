# AGENTS.md

This repository implements an out-of-tree Telegram adapter for the Orka generic gateway protocol.

## Constraints

- Never commit, print, or log Telegram bot tokens, webhook secrets, Orka bearer tokens, or Docker credentials.
- Secrets must come from environment variables, `*_FILE` variables, or Kubernetes Secrets.
- Keep Telegram-specific code outside Orka core.
- Preserve Orka delivery IDs and provide durable idempotency through SQLite.
- Run `gofmt`, `go vet ./...`, and `go test ./...` after Go changes.
- Sign commits with `git commit -s` and use Conventional Commit subjects.
