# AGENTS.md

Guidance for coding agents and contributors working **on** this repository. To
operate the CLI itself as an agent, see
[`skills/rancher/SKILL.md`](skills/rancher/SKILL.md) instead.

`rancher` is a kubectl-style, agent-first CLI for Rancher Manager. Read
[`ARCHITECTURE.md`](ARCHITECTURE.md) for the layer map and design decisions, and
[`README.md`](README.md) for the command surface.

## Build & test

Go: the version go.mod names (1.27). Automation is `make`, dispatching to
`scripts/`:

| Command | Does |
|---|---|
| `make build` | build `bin/rancher` (CGO-free) |
| `make test` | `go test -cover` on every package, with `-race` on all but `test/conformance` and `test/cachemodel`; `test/conformance` runs uncached |
| `make validate` | gofmt, vet, lint, tests (race detector except the source scans and the cache model) - the commit gate; CI runs it as two parallel jobs, `./scripts/validate static` and `./scripts/validate tests` |
| `make e2e` | envtest provider suite + testscript scenarios |
| `make e2e-fidelity` | verify recorded fidelity goldens against a real Rancher in Docker with an imported k3s downstream (~7-9 min boot; never on PRs - CI runs it on demand or weekly) |
| `make e2e-fidelity-update` | re-record the fidelity oracle goldens, downstream family included (review the diff like the schema golden) |
| `make e2e-fidelity-downstream` | compare a Rancher you run yourself, with its own downstream cluster, against the downstream goldens (never on PRs) |
| `make e2e-fidelity-downstream-update` | re-record the downstream fidelity goldens from that server instead of the pinned oracle |
| `make rancher-api-fields` | compare `internal/cli/testdata/rancher-api-fields.golden.json` with the Rancher API types at the commits the oracle tags point at, pseudo-versions included (needs the Go module proxy and GitHub; runs weekly in e2e-fidelity.yaml) |
| `make rancher-api-fields-update` | rewrite that golden after an oracle image bump (review the diff like the schema golden) |
| `make dev` | run the binary hermetically (temp config dir, file credential store) |

`make` with no target runs `ci` (the full pipeline).

Gotchas:

- **The source scans never come from the test cache.** `test/conformance`
  lists the repository's files through git, which Go's test cache does
  not see, so `scripts/test` runs it with `-count=1`.
- **The e2e suite is behind a build tag** - a bare `go test ./test/e2e/` silently runs
  nothing. Use `make e2e`.
- **Never run `bin/rancher` bare.** Use `make dev`, or prefix
  `RANCHER_CLI_CONFIG_DIR=$(mktemp -d) RANCHER_CLI_CREDENTIAL_STORE=file`. A bare
  run writes to the real OS keychain and config directory.
- **The fidelity oracle never runs on PRs.** `make e2e-fidelity` needs Docker and
  boots the pinned `rancher/rancher` image; CI runs it only on demand
  (`workflow_dispatch` on e2e-fidelity.yaml) or on its weekly schedule. The image
  pins (the oracle and the k3s downstream it imports) live in
  `scripts/e2e-fidelity`; bumping either is a reviewed golden-regeneration
  change.
- **Upcoming Rancher minors get their own oracle.**
  `E2E_FIDELITY_MINOR=<major>.<minor>` (for example `2.16`) boots the
  newest build of a minor with no GA release yet:
  `rancher/rancher:v<minor>-head` once `release/v<minor>` exists, else
  `rancher/rancher:head` when its `CATTLE_SERVER_VERSION` is of that
  minor; anything else stops the run before boot. The downstream is the
  k3s that build names in `CATTLE_K3S_VERSION`. Both are resolved to
  digests and recorded in the state file. `E2E_FIDELITY_UPCOMING_IMAGE`
  and `E2E_FIDELITY_K3S_IMAGE` (each `repo:tag@sha256:<hex>`) override
  them; `E2E_FIDELITY_PRINT_IMAGES=1` prints the choice and boots
  nothing. Each minor has its own port, subnet, containers and state
  root from the slot table in `scripts/e2e-fidelity`; a minor with no
  row is refused. It runs the boot smoke; with `E2E_FIDELITY_DRIFT=1` it
  also compares every probe with the community goldens and writes a
  report (exit 0 clean, 3 differences, any other code a boot or harness
  failure), matching differences against
  `internal/testrancher/fidelity/testdata/upcoming/v<minor>.json`.
  `E2E_FIDELITY_UPCOMING_IMAGE`, `E2E_FIDELITY_K3S_IMAGE`,
  `E2E_FIDELITY_PRINT_IMAGES=1` and `E2E_FIDELITY_DRIFT` are refused
  without `E2E_FIDELITY_MINOR`.
- **The downstream fidelity axis rides on `make e2e-fidelity`.** The oracle
  imports a pinned `rancher/k3s` container on every boot and exports
  `E2E_FIDELITY_DOWNSTREAM`, so probes marked `Downstream: true` (family
  `access-downstream`) record and verify against the pinned image like
  every other family. `scripts/e2e-fidelity-downstream` (and its `-update`
  twin) compares a Rancher you run yourself, with its own downstream
  cluster, against the same goldens: `E2E_FIDELITY_URL`,
  `E2E_FIDELITY_ADMIN_TOKEN`, `E2E_FIDELITY_CACERT` (a PEM, or the literal
  `system` to trust your OS root store), `E2E_FIDELITY_DOWNSTREAM` (the
  cluster id). Never on PRs.

## Invariants - do not break these

- **The surface describes itself.** `rancher schema` emits a versioned manifest of
  every command and flag, golden-tested at
  `internal/cli/testdata/schema.golden.json`. A surface change fails the build
  until you regenerate the golden on purpose:
  `go test ./internal/cli/ -run TestSchemaManifestGolden -update`. Unexpected drift
  is a signal, not noise. COMMANDS.md's surface facts (the counts paragraph, the
  Commands table) are conformance-tested against the schema manifest - a failure
  there means update COMMANDS.md, not the test.
- **One safety pipeline for every mutation.** Server-side dry-run → an `Effect` →
  explicit gates (`--apply`, `--yes` for destructive, `--allow-deprecated`) →
  execute, bound to the preview by a resourceVersion precondition. A command
  cannot weaken its own gates.
- **A Local command writes to the server only objects the CLI created for its
  own use and can prove it created.** Anything else is a resource mutation and
  goes through the gated pipeline (`rancher delete token <name> --apply --yes`).
  Proof is the provenance plus `tokenRef` the CLI recorded at creation, and a
  bearer `usableBearer` binds to that ref. The revoke entry points are called
  only from the files `TestTokenRevokesOnlyFromTheFilesThatMayDeleteTokens`
  (test/conformance/revokecallers_test.go) lists.
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
  meaning (test/conformance/planningrefs_test.go enforces this).
  Test doc-comments say "asserts", never "pins": "pin" is
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

## Cutting a release

- Pushing a tag `vX.Y.Z` or `vX.Y.Z-{alpha|beta|rc}.N` on a commit that is already on main
  (`scripts/check-release-tag <tag>` checks the format first) runs `.github/workflows/release.yaml`:
  zizmor on the tagged commit, then `make validate` and `make e2e`, then the build, the attestations and a draft
  release that holds every asset, with the tag as its title. No release is published yet.
- Review the draft in the GitHub UI: the assets and `SHA256SUMS`. Paste the reviewed notes into the draft, in the UI or
  with `gh release edit <tag> --notes-file <file>`, before publishing: watchers' notifications and the releases feed
  carry the body as it is at publish.
- Publish with `gh release edit <tag> --draft=false --prerelease=false --latest` (or in the UI). A draft for a tag
  with a suffix starts as a pre-release; the command publishes it as not a pre-release and as the latest release.
- After publishing, the title and notes can still be edited; the release cannot return to a draft. Assets cannot be
  added or changed, and a bad release is fixed only by a new version. `scripts/release-upsert` refuses to upload to a
  published release. Deleting a published release does not free its tag name: GitHub refuses a new release with that name.
- Never move, delete and push again, or reuse a pushed tag. A failed cut uses up its number.
- A run that fails before publishing: re-run the failed jobs when the cause is a flake (the upload to the draft is
  idempotent). Otherwise delete the draft and the tag, fix on main, and cut the next number.

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
