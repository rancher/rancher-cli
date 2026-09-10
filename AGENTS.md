# AGENTS.md

Guidance for coding agents and contributors working **on** this repository. To
operate the CLI itself as an agent, see
[`skills/rancher/SKILL.md`](skills/rancher/SKILL.md) instead.

`rancher` is a kubectl-style, agent-first CLI for Rancher Manager. Read
[`ARCHITECTURE.md`](ARCHITECTURE.md) for the layer map and design decisions, and
[`README.md`](README.md) for the command surface.

## Build & test

Go 1.27.1+. Automation is `make`, dispatching to `scripts/`:

| Command | Does |
|---|---|
| `make build` | build `bin/rancher` (CGO-free) |
| `make test` | `go test -race -cover ./...` |
| `make validate` | gofmt, vet, lint, race - the commit gate |
| `make e2e` | envtest provider suite + testscript scenarios |
| `make e2e-real` | verify recorded fidelity goldens against a real Rancher in Docker with an imported k3s downstream (~7-9 min boot; never on PRs - CI runs it on demand or weekly) |
| `make e2e-real-update` | re-record the fidelity oracle goldens, downstream family included (review the diff like the schema golden) |
| `make e2e-real-downstream` | compare a Rancher you run yourself, with its own downstream cluster, against the downstream goldens (never on PRs) |
| `make e2e-real-downstream-update` | re-record the downstream fidelity goldens from that server instead of the pinned oracle |
| `make dev` | run the binary hermetically (temp config dir, file credential store) |

`make` with no target runs `ci` (the full pipeline).

Two gotchas:

- **The e2e suite is behind a build tag** - a bare `go test ./e2e/` silently runs
  nothing. Use `make e2e`.
- **Never run `bin/rancher` bare.** Use `make dev`, or prefix
  `RANCHER_CLI_CONFIG_DIR=$(mktemp -d) RANCHER_CLI_CREDENTIAL_STORE=file`. A bare
  run writes to the real OS keychain and config directory.
- **The fidelity oracle never runs on PRs.** `make e2e-real` needs Docker and
  boots the pinned `rancher/rancher` image; CI runs it only on demand
  (`workflow_dispatch` on e2e-real.yaml) or on its weekly schedule. The image
  pins (the oracle and the k3s downstream it imports) live in
  `scripts/e2e-real`; bumping either is a reviewed golden-regeneration
  change.
- **The downstream fidelity axis rides on `make e2e-real`.** The oracle
  imports a pinned `rancher/k3s` container on every boot and exports
  `E2E_REAL_DOWNSTREAM`, so probes marked `Downstream: true` (family
  `access-downstream`) record and verify against the pinned image like
  every other family. `scripts/e2e-real-downstream` (and its `-update`
  twin) compares a Rancher you run yourself, with its own downstream
  cluster, against the same goldens: `E2E_REAL_URL`,
  `E2E_REAL_ADMIN_TOKEN`, `E2E_REAL_CACERT` (a PEM, or the literal
  `system` to trust your OS root store), `E2E_REAL_DOWNSTREAM` (the
  cluster id). Never on PRs.

## Invariants - do not break these

- **The surface describes itself.** `rancher schema` emits a versioned manifest of
  every command and flag, golden-tested at
  `surface/cli/testdata/schema.golden.json`. A surface change fails the build
  until you regenerate the golden on purpose:
  `go test ./surface/cli/ -run TestSchemaManifestGolden -update`. Unexpected drift
  is a signal, not noise. COMMANDS.md's surface facts (the counts paragraph, the
  Commands table) are conformance-tested against the schema manifest - a failure
  there means update COMMANDS.md, not the test.
- **One safety pipeline for every mutation.** Server-side dry-run → an `Effect` →
  explicit gates (`--apply`, `--yes` for destructive, `--allow-deprecated`) →
  execute, bound to the preview by a resourceVersion precondition. A command
  cannot weaken its own gates.
- **A resource is a reviewed table row.** Add a kind to `internal/engine/kinds`;
  get/patch/delete are generated from it. Reach for bespoke code only when a row
  cannot express it.

## Conventions

- `docs/` is gitignored - never stage anything under it.
- **Commits:** sentence-style subject, no Conventional-Commits prefix; a body that
  states the prior behavior, then the change. History is linear - no merge
  commits.
- **No attribution.** Nothing names an assistant, model, or automated tool as
  an author or contributor: not in source, docs, reviews, generated artifacts,
  or commit messages (no Co-Authored-By or similar trailers). Dependency
  bots (Renovate, Dependabot) are the exception: their trailers stay.
- **Tests:** testify; a real server (`httptest`) or dependency injection over
  mocks; any test touching keyring code calls `keyring.MockInit()` first. Never
  string-match rendered JSON - decode the envelope and assert on fields.
  Error substrings: `require.ErrorContains(t, err, ...)`, never
  `assert.Contains(t, err.Error(), ...)` (a source-scan test enforces
  this); the negative form `NotContains(t, err.Error(), ...)` is the
  sanctioned spelling - testify has no ErrorNotContains.
- **Comments are self-contained.** A comment states a constraint the
  code can't show - never its provenance. No references to design
  docs, reviews, rounds or findings, handoffs, recordings, tasks, or
  commit hashes; a reader with only the repo must get the full
  meaning. Test doc-comments say "asserts", never "pins": "pin" is
  reserved for the credential-store and CA pin features and for
  dependency pins (images, tools, modules).
- **Descriptions:** operator-facing help is terse and plain; the schema `desc`
  carries the fuller, precise wording. Keep internal jargon out of both:
  plain verbs (create/generate - never "mint", in identifiers and comments
  as well as user-facing text; the token a login creates for storage is
  "the derived token"), no coined terms; for anything Kubernetes or
  Rancher names, use their term (the API field name or their docs'
  phrase) and never a synonym of our own. Established internal vocabulary
  (probe, seam, envelope) lives in code and comments only.
- **Plain spelling, one dash.** Normal spelling everywhere ("facade",
  never an accented letter) and the ASCII hyphen for every dash: `word -
  word` for an aside, `5-7` for a range; no em-dash or en-dash. In
  monospace source and CLI output the three dashes look the same, and the
  hyphen is the plain-text convention. Decorative characters (the middle
  dot, arrows, the ellipsis) are fine. A source-scan test enforces the
  rule; runtime text from users and servers is rendered verbatim and is
  out of its scope.
- **Completion ships with the surface.** A change that adds or alters a
  value-taking flag or argument includes its shell completion (static enum
  or live fetch) and completion tests in the same change. A suggestion must
  be valid verbatim - never offer a value the command would reject or
  resolve ambiguously.
- **Modern Go for the pinned toolchain.** `t.Context()` over
  `context.Background()` in tests, and current stdlib idioms (`slices`,
  `maps`, `strings.Cut*`/`SplitSeq`, `errors.AsType`, `sync.OnceValue`,
  `WaitGroup.Go`, and so on) elsewhere. New code is always written to the
  current idiom - never matched down to a stale one already in the file.

## Knowledge graph (graphify)

The repo carries a local knowledge graph at `graphify-out/` (gitignored,
per-machine) with god nodes, community structure, and cross-file
relationships.

- For codebase questions, first run `graphify query "<question>"` when
  `graphify-out/graph.json` exists. Use `graphify path "<A>" "<B>"` for
  relationships and `graphify explain "<concept>"` for focused concepts -
  these return a scoped subgraph, much smaller than raw grep output.
- Read `graphify-out/GRAPH_REPORT.md` only for broad architecture review or
  when query/path/explain do not surface enough context.
- After modifying code, run `graphify update .` to keep the graph current
  (AST-only, no API cost).

## Before claiming done

- `make validate` passes (the pre-commit gate).
- `make e2e` before merging anything that touches the command surface.
- Cross-compile `GOOS=windows` and `GOOS=linux` before merging platform-adjacent
  changes.
