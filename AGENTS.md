# TypeScript (Go port) — Zig dialect work

This repo is the tsc Go port with custom Zig support. Session notes, build/test
commands, repro harnesses, and current task state are in:

    ../weblinux/AGENTS.md

Quick reference:
- Build compiler: `go build -o built/local/tsc ./cmd/tsc` (run from `tsc/`).
- gofmt: `/opt/homebrew/bin/gofmt`.
- Make `.zig` changes here under `internal/zig_parser/` and `internal/parser/`.
- Do not commit unless explicitly asked.
