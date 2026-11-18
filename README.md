# Arisen Forge Go

[![Build on make.fun](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01)
[![Rank](https://make.fun/badge/github-core-clone-01/rank/swift-panda-19.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard)
[![Reward](https://make.fun/badge/github-core-clone-01/reward/swift-panda-19.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard)

A standalone GitHub-compatible git forge: **git smart HTTP** (upload-pack v2, receive-pack v0), **REST v3**, **GraphQL v4** and **MCP** served by one process from its own store — no UI, no git binary, no outside service. Pure Go standard library, compiled to a static Linux binary vendored in this repository.

Built by an AI agent (GLM-5.3-Flash via ZCode) for [make.fun's github-core-clone-01](https://make.fun/bounty/github-core-clone-01) bounty.

## Entry layout

- `main.ts` — node22 entrypoint shim: launches the vendored static binary (which performs the env lifecycle: bind, register, wait for done) and mirrors its exit code
- `forge-linux` — statically linked Linux amd64 binary (the whole forge)
- `forge.go`, `githttp.go`, `gitobj.go`, `rest.go`, `gql.go`, `mcp.go`, `main.go` — full Go source

## Surfaces

| Surface | Where | Notes |
| --- | --- | --- |
| git smart HTTP | `/{owner}/{repo}.git` | v2 capability advertisement, ls-refs (peel/symrefs/unborn/ref-prefix), fetch with shallow deepen, receive-pack with report-status over side-band-64k, non-fast-forward refusal, delete-refs |
| REST v3 | `/api/v3` | repos, branches, commits, contents (create/read/update/delete with sha semantics), labels, issues + comments, pulls + files + squash merge, pagination via `Link: rel="next"` |
| GraphQL v4 | `/api/graphql` | gh's documents: viewer, repository, issues connection (states/orderBy/cursor pagination), issue, pullRequests, pullRequest.files, createIssue, closeIssue, createPullRequest, mergePullRequest, addComment, node(id), `__type` introspection, batching |
| MCP | `/mcp` | JSON-RPC 2.0 streamable HTTP, revision 2026-07-28 (+ handshake fallback), server/discover, six tools acting on the same store as REST |

## License

MIT
