# AGENTS.md

Rules for AI agents and human collaborators working on Lightning Jet 2.0. Read
this before changing anything.

Jet decides fees and moves liquidity on routing nodes that hold real money.
Treat every change as though it will run unattended against a funded node,
because eventually it will.

## What this is

Jet 2.0 is a ground-up rebuild in Go, on the `v2` branch. It shares no history
with Jet 1.7, which lives on `main` and `dev` and is frozen. Work on 2.0 never
touches those branches.

Read these first:

- [`docs/architecture/overview.md`](docs/architecture/overview.md): the design.
- [`docs/architecture/decision-contracts.md`](docs/architecture/decision-contracts.md):
  the records every decision produces, and the three invariants they obey.
- [`docs/reference/jet-1.7-internals.md`](docs/reference/jet-1.7-internals.md):
  what 1.7 actually does, as reference. Don't port its code.

## Ground rules

- **Read a file before editing it.** Edit what is on disk, not what you assume.
- **Every dependency must justify its existence.** A new module needs a written
  reason in the pull request, and `go.sum` pins its hash. Prefer the standard
  library, and prefer copying twenty lines over adding a module.
- **LND over REST, with the standard library.** Don't add gRPC, protobuf, code
  generation or LND's own Go packages. A new LND call gets a hand-written type
  and a test against a synthetic LND response.
- **No cgo.** Everything builds with `CGO_ENABLED=0` for the platforms nodes run
  on.
- **The core makes no outbound connections beyond the node's own LND.** Optional
  integrations, notifications included, run as separate processes, are opt-in,
  and are never on the path that manages funds.
- **No telemetry, ever.** Operational data stays on the operator's machine.
- **Never commit secrets, key material or real node data.** That includes seeds,
  macaroons, certificates, wallet files and `.env` contents. It also includes
  anything taken from a real node: pubkeys, channel ids, channel points,
  balances, fees, forwards and earnings. Anonymized exports don't count as safe,
  because a real node's channels can be identified from the public graph.
  - Not in code, tests, fixtures, examples, docs or commit messages.
  - Fixtures use obviously synthetic values, like the pubkey `02aaaa…` or the
    channel id `111111111111111111`.
  - Real data for local analysis lives under `local/`, which is never committed.
- **Never test against real funds.** Use the fake LND, regtest or signet. A change
  that cannot be exercised without mainnet money is not ready.
- **Distinguish observed from derived.** A value read from LND and a value Jet
  inferred never share a field.
- **Keep the AI out of enforcement.** A strategy or model may propose, estimate or
  explain. Deterministic code authorizes, and limits are never set or relaxed by
  a model.
- **Make the smallest change that works.** No refactors, reformatting or renames
  the task doesn't need.

## Go conventions

- `gofmt`, `go vet` and `go test -race` are clean before every commit.
- **Money.** Amounts are millisatoshis in a typed 64-bit integer, never floating
  point. Channel ids are 64-bit integers, written as decimal strings in JSON.
- **LND calls.** Every LND call takes a `context.Context` with a deadline.
  Nothing blocks without one.
- **Determinism.** Time, randomness and identifiers are injected, so tests are
  deterministic and measure behavior rather than the host.
- **Package layout.** Packages live under `internal/` unless something outside the
  module must import them.

```bash
gofmt -l .                  # lists unformatted files; must print nothing
go vet ./...
go test -race ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath ./cmd/jet
```

## Economics

Profitability is the objective, not a promise. Any claim that a strategy performs
better must say against which baseline (doing nothing, static rules, or 1.7),
over what period, net of which costs, and on which kind of node. Simulated and
replayed results are demonstrations, never evidence of profit on a real node.

## Branches and releases

- Work goes on `v2`, directly or through topic branches off it. Never commit to
  `main` or `dev` from 2.0 work.
- Tags are plain semver: `v2.0.0`, with pre-releases like `v2.0.0-alpha.1`. A
  release's theme goes in its title, never in the tag.
- **A pushed tag is never moved or deleted.** Go's checksum database records every
  version permanently, so a moved tag breaks every user who fetched it.

## Attribution

Jet 2.0 is AI-native in how it is built: **every commit is written with AI
assistance, and every commit carries a `Co-Authored-By` trailer** naming the
model that did the work, with the vendor's noreply address where one exists:

```
Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
```

A commit without a trailer is incomplete, the same as a commit without a message,
and CI checks every commit on the branch for one.
Name the specific model, not a placeholder; add more than one trailer if more
than one model was used. The commit's author is the human who directed the work.

A pull request whose code was substantially AI-generated says so in its
description and names the tool, conventionally as a footer:

🤖 Generated with [Claude Code](https://claude.com/claude-code)

## Before you finish

1. `gofmt`, `go vet` and `go test -race` pass, and you have said what you ran.
2. Any new dependency has a written reason.
3. No secrets, key material, real node data, absolute paths or machine names
   appear anywhere in the diff.
4. Observed and derived values are still distinguishable.
5. Nothing reaches the network that did not before.
6. Every commit carries its `Co-Authored-By` trailer.
