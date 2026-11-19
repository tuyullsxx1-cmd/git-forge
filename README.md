# forge — a GitHub-compatible git forge

A single-process, dependency-free GitHub-compatible server: REST v3,
smart-HTTP git (upload-pack v2 / receive-pack v0), GraphQL and MCP,
implemented from scratch in Go. The git object format (loose objects,
packfiles, deltas, pkt-lines) is implemented by hand — no `git` binary is
invoked at runtime.

Built for the make.fun bounty
[Build a GitHub-compatible git forge](https://make.fun/bounty/github-core-clone-01).

## Layout

| Path | What it is |
| --- | --- |
| `main.py` | Entry point. Thin stdlib-only launcher: execs the vendored `forge` binary next to it. |
| `forge` | Static Go binary (6.5 MiB, `-trimpath -ldflags="-s -w"`), the whole server. |
| `src/` | Go source (module `forge`): `main.go` (boot/wire), `store.go` (in-memory store, merge, fetch), `gitobj.go` (objects, packs, deltas), `gitproto.go` (pkt-line, v2/v0, receive-pack), `rest.go` (REST v3), `graphql.go` (GraphQL executor), `mcp.go` (MCP JSON-RPC), `scenario.go` (deterministic viewer/scenario). |
| `.makefun/owner.json` | Entry owner wallet. |
| `DESIGN.md` | Architecture and the top-10 patterns this entry follows. |

## Running

```sh
python3 main.py
```

The launcher starts `./forge`, which:

1. Binds an ephemeral port (`127.0.0.1:0`).
2. `POST {"port": N}` to `$MAKEFUN_ENV_URL/register` within 20 s
   (auth: `MAKEFUN_SHOP_TOKEN`).
3. Answers `GET /healthz` with 200.
4. Long-polls `GET $MAKEFUN_ENV_URL/done` until `{"done": true}`, then exits 0.

Without `MAKEFUN_ENV_URL` it runs standalone on `FORGE_PORT`
(default: an ephemeral port) with `MAKEFUN_SHOP_TOKEN` (default: unset,
auth disabled) — useful for local smoke tests.

## Building from source

```sh
cd src
go build -buildvcs=false -trimpath -ldflags="-s -w" -o ../forge .
```

Requires Go 1.25+. No third-party dependencies: the module uses only the
Go standard library.

## What it implements

- **REST `/api/v3`** — root/meta/user, user repos, repo CRUD, branches,
  commits, contents (base64, 201/200/409), labels, issues (+ comments),
  pulls (+ files, squash merge), git refs. JSON everywhere, `X-GitHub-Request-Id`
  on every reply, 401 without a bearer token, 404 (never 403) for another
  owner's repo.
- **Git smart HTTP `/{owner}/{repo}.git`** — upload-pack protocol v2
  (ls-refs, fetch, shallow/deepen, ack negotiation), receive-pack v0
  (report-status, side-band-64k, delete-refs, fast-forward only).
  Loose + packfile objects, `OBJ_OFS_DELTA` / `OBJ_REF_DELTA`, annotated-tag
  peeling, fetch pack cache.
- **GraphQL `POST /api/graphql`** — viewer, repository, issues/pullRequests
  connections (cursor pagination, `CREATED_AT DESC`), mutations
  (create/close issue, create/merge pull request, addComment), `node(id:)`,
  `__type` introspection, batched operations, unknown-field errors.
- **MCP `POST /mcp`** — JSON-RPC 2.0 Streamable HTTP, revision 2026-07-28
  (`server/discover`) and legacy 2025-11-25 handshake, 6 tools
  (`create_repo`, `get_repo`, `create_issue`, `list_issues`,
  `create_pull_request`, `merge_pull_request`), byte-identical `tools/list`.
- **Determinism** — fixed epoch timestamps, sequential ids, no wall clock or
  unseeded randomness in state; two runs on the same instance export
  byte-identical state.

## Local grade

Graded locally against the kit's own environment (`program/env/server.mjs`,
no Docker) — 4 runs, all **120/120 conformance checks** (git 30, repos 15,
issues 15, pulls 20, graphql 20, mcp 15, protocol 5) and **186–189/200**
performance points:

| Workload | House T_ref | This forge (p50) | Points |
| --- | --- | --- | --- |
| push (200× 1 KiB) | 1640.82 ms | 288–306 ms | 50 |
| clone (50 concurrent, depth 1, 200 commits) | 266.94 ms | 66–75 ms | 48–50 |
| read (2000 GET issue, keep-alive) | 2759.72 ms | 604–728 ms | 49–50 |
| write (200 POST issue) | 348.97 ms | 91–105 ms | 47–49 |

Estimated total: **600 + 200 + ~188 = ~988/1000** (best observed run: 199/200 perf).
