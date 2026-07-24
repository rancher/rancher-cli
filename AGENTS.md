# AGENTS.md

Guidance for coding agents and contributors working **on** this repository. To
operate the CLI itself as an agent, see
[`skills/rancher/SKILL.md`](skills/rancher/SKILL.md) instead.

`rancher` is a kubectl-style, agent-first CLI for Rancher Manager. Read
[`ARCHITECTURE.md`](ARCHITECTURE.md) for the layer map and design decisions, and
[`README.md`](README.md) for the command surface.

## Build & test

Go 1.26+. Automation is `make`, dispatching to `scripts/`:

| Command | Does |
|---|---|
| `make build` | build `bin/rancher` (CGO-free) |
| `make test` | `go test -race -cover ./...` |
| `make validate` | gofmt, vet, lint, race — the commit gate |
| `make e2e` | envtest provider suite + testscript scenarios |
| `make dev` | run the binary hermetically (temp config dir, file credential store) |

`make` with no target runs `ci` (the full pipeline).

Two gotchas:

- **The e2e suite is behind a build tag** — a bare `go test ./e2e/` silently runs
  nothing. Use `make e2e`.
- **Never run `bin/rancher` bare.** Use `make dev`, or prefix
  `RANCHER_CLI_CONFIG_DIR=$(mktemp -d) RANCHER_CLI_CREDENTIAL_STORE=file`. A bare
  run writes to the real OS keychain and config directory.

## Invariants — do not break these

- **The surface describes itself.** `rancher schema` emits a versioned manifest of
  every command and flag, golden-tested at
  `surface/cli/testdata/schema.golden.json`. A surface change fails the build
  until you regenerate the golden on purpose:
  `go test ./surface/cli/ -run TestSchemaManifestGolden -update`. Unexpected drift
  is a signal, not noise. README's surface facts (the counts paragraph, the
  Commands table) are conformance-tested against the schema manifest — a failure
  there means update README.md, not the test.
- **One safety pipeline for every mutation.** Server-side dry-run → an `Effect` →
  explicit gates (`--apply`, `--yes` for destructive, `--allow-deprecated`) →
  execute, bound to the preview by a resourceVersion precondition. A command
  cannot weaken its own gates.
- **A resource is a reviewed table row.** Add a kind to `internal/engine/kinds`;
  get/patch/delete are generated from it. Reach for bespoke code only when a row
  cannot express it.

## Conventions

- `docs/` is gitignored — never stage anything under it.
- **Commits:** sentence-style subject, no Conventional-Commits prefix; a body that
  states the prior behavior, then the change. No Co-Authored-By attribution.
  History is linear — no merge commits.
- **Tests:** testify; a real server (`httptest`) or dependency injection over
  mocks; any test touching keyring code calls `keyring.MockInit()` first. Never
  string-match rendered JSON — decode the envelope and assert on fields.
- **Descriptions:** operator-facing help is terse and plain; the schema `desc`
  carries the fuller, precise wording. Keep internal jargon out of both.
- **Completion ships with the surface.** A change that adds or alters a
  value-taking flag or argument includes its shell completion (static enum
  or live fetch) and completion tests in the same change. A suggestion must
  be valid verbatim — never offer a value the command would reject or
  resolve ambiguously.

## Before claiming done

- `make validate` passes (the pre-commit gate).
- `make e2e` before merging anything that touches the command surface.
- Cross-compile `GOOS=windows` and `GOOS=linux` before merging platform-adjacent
  changes.
