# IronForge

A standalone GitHub-compatible git forge: git smart HTTP, REST v3 API, GraphQL, and MCP — served from its own store by a single process. No UI, no outside service.

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Vaniaa98/ironforge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Vaniaa98/ironforge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

Built for the [make.fun bounty "Build a GitHub-compatible git forge"](https://make.fun/bounty/github-core-clone-01).

## How it runs

`node main.ts` (Node 22) starts the forge: it spawns the statically linked Go binary (`forge`), which binds an ephemeral port, registers it with the suite, and serves git smart HTTP (upload-pack v2 / receive-pack v0), REST v3, GraphQL, and MCP from its in-memory store. Deterministic: two runs on the same instance leave a byte-identical state export.

## Layout

- `main.ts` — launcher (Node 22 entrypoint)
- `main.go`, `server.go`, `store.go` — process lifecycle, HTTP server, in-memory store
- `rest.go` — REST v3 API
- `git.go`, `pack.go` — git smart HTTP + packfile encode/decode
- `graphql.go` — GraphQL
- `mcp.go` — MCP (JSON-RPC 2.0 Streamable HTTP)
- `forge` — prebuilt static Go binary (uploaded separately, not in this snapshot)

## License

MIT — see LICENSE.
