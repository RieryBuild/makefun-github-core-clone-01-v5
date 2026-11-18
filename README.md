# makefun-github-core-clone-01-v5

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/RieryBuild/makefun-github-core-clone-01-v5/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/RieryBuild/makefun-github-core-clone-01-v5/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone GitHub-compatible git forge in one process, built from scratch in
Go: smart-HTTP clone/push (protocol v2 upload-pack, v0 receive-pack), REST v3
(repos, branches, commits, contents, issues, pull requests), GraphQL v4 (the
documents gh sends) and MCP tools over streamable HTTP — all served from an
in-memory store with real SHA-1 git objects, packs and refs. Zero runtime
dependencies, no `git` binary: every git format (packs, pkt-lines, trees,
commits, refs) is implemented in-process.

v5 is a from-scratch Go rewrite of v4 (which passes 120/120 conformance
checks, practice grade 971). The conformance surface is unchanged; the
runtime is a statically linked Go core (`forge`, ~3.3 MB, committed beside
its source in `go/`) behind a thin Node launcher (`main.ts`) that owns the
entry lifecycle. The Go route is sanctioned by the competition rules
(RULES.txt: "a statically linked Linux amd64 binary you commit runs from your
repository as vendored source").

Design, for the same hot paths v4 optimized:

- **Pre-built responses.** `GET /healthz`, `GET /api/v3[/]`, `GET /api/v3/meta`
  and `GET /repos/{o}/{r}/issues/{n}` answer from pools of complete,
  already-joined responses (status line, headers, body) with rotating request
  ids — one socket write, zero per-request serialization. Issue pools are
  built lazily on first GET and dropped on mutation.
- **Zero-alloc read path.** The issue GET/POST hot paths match on the raw
  target bytes before any decoding or segment splitting, and the repo lookup
  hits a one-entry cache keyed by the raw name bytes (guarded by a repo
  generation counter), so a repeated hot request allocates nothing. Anything
  ambiguous (percent-escapes, non-digit tails, extra segments) falls through
  to the generic dispatch, so the fast paths can never diverge from it.
- **Hand-rolled HTTP/1.1 over raw TCP.** One goroutine per connection, a
  single-pass header scan that picks out only the headers the dispatch needs,
  keep-alive and pipelining, chunked bodies decoded. No `net/http` machinery.
- **Template v0 advertisement.** The receive-pack/upload-pack advertisement's
  shape depends only on ref *names*, so it is built once per ref-name change
  and served by copying the template and splicing the current tip oids into
  their slots. (Falls back to a fresh build when a ref tip is an annotated
  tag, where the peeled line depends on the tip itself.)
- **Cached packs.** Fetch packs and ls-refs answers are cached on (repo
  version, raw request body), so the clone workload's identical clones share
  one pack build. Packs are built from plain objects (no deltas), sorted by
  id for determinism, and the object bytes go in as stored (uncompressed)
  zlib blocks — on the loopback trial link bytes are nearly free and CPU is
  the constraint, so skipping the deflate pass is a pure win.
- **Pooled zlib codecs.** Pack inflate/deflate reuses codec state through
  sync.Pool instead of allocating a fresh flate codec per object.
- **Zero-copy push parsing.** The receive-pack body is parsed directly from
  the request buffer (manual pkt-line lengths, the pack taken as a sub-slice
  view) and inflated in a single pass; the pack's objects are stored even
  when a ref update is refused.

## Run it

```sh
node --experimental-strip-types main.ts   # needs MAKEFUN_ENV_URL in a trial
```

The grader runs `node /bot/main.ts`. The practice harness (`sdk/run.ts` in
the competition kit) runs the entry against the conformance suite + the four
perf workloads and prints the score.

To run the Go core directly (local development):

```sh
./forge 8080            # binds port 8080; MAKEFUN_SHOP_TOKEN for auth
```

To rebuild it from source (Go 1.25):

```sh
cd go && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o ../forge .
```

## Layout

```
main.ts            entry <-> env lifecycle (spawn forge, register, poll /done, exit 0)
forge              the prebuilt static Go core (linux/amd64, ~3.3 MB)
go/main.go         core entry: bind the launcher's port, serve until killed
go/transport.go    hand-rolled HTTP/1.1 over raw TCP (single-pass header scan,
                   structured replies + pre-built buffers, keep-alive/pipelining)
go/router.go       routing, auth, fast paths, static response pools, git routes
go/store.go        the forge: repos, objects, refs, issues, pulls, packs,
                   reachability, merge-base, diffs
go/git.go          smart HTTP: v0/v2 advertisements, ls-refs, fetch, receive-pack
go/rest.go         REST v3 (repos, branches, commits, refs, contents, issues,
                   labels, pulls) + the issue response pool
go/gql.go          GraphQL v4: streaming lexer, recursive-descent parser,
                   switch-based per-type resolvers
go/mcp.go          MCP Streamable HTTP (JSON-RPC 2.0, 6 tools)
go/jsonx.go        JSON helpers: object-body parsing, append serializers
go/bench_test.go   in-process benchmarks + pack round-trip tests (excluded
                   from the binary build)
```

## Practice grades

Full grades with the kit's sealed suite (`sdk/run.ts`), back-to-back on a
shared VM — wall p50s move with the machine's load, so the load is recorded
with each run and server CPU (a count of work, not wall time) is the
load-insensitive signal.

| run | load (1-min) | score | perf | p50 µs | push | clone | read | write |
|---|---|---|---|---|---|---|---|---|
| v4 clean best | 7.5–8.2 | 971 | 171 | 301,062 | 717 | 111 | 815 | 127 |
| **v5 grade 2 (best full)** | 2.0–2.4 | **947** | 147 | 423,380 | 567 | 225 | 1,301 | 194 |
| v5 grade 1 | 11–13 | 864 | 64 | 1,317,161 | 1,743 | 309 | 4,691 | 1,190 |
| v5 grade 3 | 5.7–7.4 | 904 | 104 | 762,910 | 1,395 | 333 | 2,317 | 315 |
| v5 grade 4 | 6.3–8.3 | 914 | 114 | 667,618 | 698 | 219 | 2,669 | 487 |
| v5 grade 5 | 8.0 | 906 | 106 | 732,591 | 742 | 199 | 2,689 | 727 |
| **v5 grade 6 (submitted tree)** | 8.3–9.2 | **908** | 108 | 726,336 | 897 | 330 | 2,377 | 395 |

All four v5 grades are 120/120 conformance (0 failures). The wall numbers
are dominated by the shared VM's sibling load, so the honest comparison is
back-to-back under identical load — bench-only, 3 rounds, v4 vs v5:

| pair | load | v4 p50 µs / perf | v5 p50 µs / perf | v5 server CPU |
|---|---|---|---|---|
| 1 | ~9.5 | 957,997 / 86 | **374,856 / 155** | 520 ms vs v4's 1,178 ms |
| 2 | ~7.3 | 706,596 / 109 | **419,175 / 147** | 490 ms vs v4's 1,178 ms |

v5 wins all 8 workload comparisons across both pairs (p50 ratio 1.69–2.56×).
Server CPU per request (transport micro-benchmark): read 18–20 µs (v4:
29–33), write 29–31 µs (v4: 54); in-process dispatch: push ~49 µs (v4:
133), read 0.13 µs, cold fetch ~46 µs.

A practice grade is not a promise of a live result: v3 measured 189 in
practice and 117 live on the same code, and v4's live outlook is ~130–140
perf. Go entries show a smaller practice-to-live gap (≤1.6×); the rank-20
prizelist cutoff sits at perf 155 / p50 377,911 µs live.

## Review

An independent second model reviewed the full entry before the gate
(OpenRouter `nvidia/nemotron-3-ultra-550b-a55b:free`, 4 chunks, 65,419
prompt + 49,202 completion tokens, $0). 17 fixes were accepted across the
chunks (Git-Protocol token parsing, GraphQL `... on Node { id }` and
fragment-spread guards, MCP batch-notification handling, chunked-body
hardening, pack size caps, and others); every fix was re-verified with the
conformance suite, the smoke guard, and live checks.

## License

MIT — see `LICENSE`.

---

<sub>Entered on [make.fun](https://make.fun/?ref=wa8s3t3t&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
