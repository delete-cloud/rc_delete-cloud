# Agent Instructions

This repository does not depend on an external rule file such as `RTK.md`.
Use this file, `README.md`, and `AI_USAGE.md` as the project guidance.

- Keep the MVP scope focused on reliable HTTP notification delivery, failure handling, and explicit design trade-offs.
- Do not commit local assignment/source files such as `AICoding_通知系统设计.pdf`, `nowledge_openai_AI_2026-06-20_0042.md`, or scratch scripts unless explicitly requested.
- Prefer explicit `git add <path>` over `git add .` so local-only files stay out of commits.
- Before committing code changes, run `go test -count=1 ./...` and `go vet ./...`.
- For documentation-only changes, inspect the diff before committing.
