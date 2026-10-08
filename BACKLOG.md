# Backlog

Work identified during the `main..dev` baseline review that is deliberately
**not** being done on `chore/claude-baseline`. That branch is scoped to the
delta between `main` and `dev` only.

Each item records what was found, why it is out of scope there, and where it
should land instead.

---

## 1. `constructInsertString` builds SQL without escaping

**Severity:** high — silent data loss
**Where it belongs:** 1.7.1, on its own branch, e.g. `fix/db-sql-escaping`. Kept
out of 1.7.0 because it pre-dates that release and touches every insert.
**Why not on the baseline branch:** pre-dates the delta; `db/utils.js:962` is
unchanged since before `main`.

`db/utils.js:962`:

```js
function constructInsertString(arr) {
  return "'" + arr.join("', '") + "'";
}
```

Values are concatenated into the statement rather than bound, with no escaping.
Any single quote in a value breaks the generated SQL.

The reachable failure is `recordRebalanceFailure`, which passes `errorMsg`
straight through. That text comes from bos/LND and can contain apostrophes
(`can't route`). The chain:

1. Apostrophe breaks the INSERT
2. `executeDb(db, cmd)` is called with no callback, so the error only reaches
   the internal `if (err) logger.debug(...)` — below the default log level
3. The surrounding `try/catch` cannot catch it: `db.run` delivers errors
   asynchronously to the callback, it never throws
4. `doIt()`'s retry tests the *returned* `err`, which only the sync catch sets,
   so it is always `undefined` and no retry fires

Net: a rebalance failure whose error text contains an apostrophe is dropped
from rebalance history with no visible log. That history drives exponential
backoff and peer selection, so the loss is not cosmetic.

`recordTelegramMessageSync` already works around this narrowly, at
`db/utils.js:582`:

```js
const m = msg.replaceAll("'", '"');
```

That patches one caller and corrupts message text as a side effect
(`didn't` becomes `didn"t`).

**Suggested fix.** Escape at the single choke point using SQL-standard quote
doubling, then delete the `replaceAll` workaround so message text survives
intact:

```js
function constructInsertString(arr) {
  // values are concatenated into the statement rather than bound, so an
  // apostrophe would otherwise break the insert
  return "'" + arr.map(v => String(v).replaceAll("'", "''")).join("', '") + "'";
}
```

Parameterised queries are the proper fix, but that is a large refactor of an
intentionally unlinted legacy layer and should be costed separately.

---

## 2. Documented Node version does not match what the code needs — RESOLVED

**Resolved** in two steps. `chore/runtime-modernization` brought `README.md:27`
to 22.x+, matching `engines: { node: ">=22" }` and `.nvmrc`. That was marked
resolved too early: line 30 still installed Node 16 via `setup_16.x`, and line 36
still told users to keep npm at 8.x. Both are fixed for 1.7.0.
Retained below for context.

**Severity:** high — new users hit it on first run

Note this one **is** delta-caused. `main` contains no `styleText` and no
`replaceAll`, so it genuinely ran on Node 16 as documented. The delta raised
the floor.

| Source | Claims | Reality |
|---|---|---|
| `package.json` engines | `>=12` | wrong before and after |
| `README.md:27` | "version 16.x+" | wrong as of the delta |
| Actual code | — | **Node 20.12+** |

What forces it up:

- `cli/index.js:2` `styleText` from `node:util` — Node 20.12+
- `db/utils.js:582` `replaceAll` — Node 15+
- `api/utils.js:1-2` `node:` prefixed requires — Node 14.18+ (already on `main`)

`engines` is not enforced by npm without `engine-strict`, so `npm install`
succeeds and the failure surfaces at runtime instead — `styleText` is
`undefined` on Node 16/18 and help rendering throws.

`chore/runtime-modernization` sets `engines` to `>=22` and adds `.nvmrc`, but
does not touch the README. **Update `README.md:27` to match whatever `engines`
ends up at, in the same change.**

---

## 3. Async `executeDb` errors bypass both the catch and the retry

**Severity:** medium
**Where it belongs:** with item 1 — same code path, same branch

Independent of the escaping issue, `executeDb` errors are structurally
unreachable by the callers' error handling:

- `deleteTelegramMessages` (`db/utils.js:~600`) and `deleteProp`
  (`db/utils.js:~601`) call `executeDb(db, cmd)` with no callback at all, so
  failures are logged at `debug` and otherwise discarded
- the `doIt()` retry idiom used throughout the file only re-runs when the
  synchronous catch sets `err`, which never happens for async `db.run` errors

Worth noting the delta *improved* this line rather than regressing it: the old
code was `if (err & testMode)` — a bitwise `&` on an Error object, always
falsy, so it never logged at all. The new `if (err) logger.debug(...)` at least
records something. It is just still below the default threshold.

---

## 4. `api/telegram.js` has no throttling or deduplication

**Severity:** low — robustness
**Where it belongs:** its own branch

`sendMessage` sends unconditionally. There is no rate limit and no dedup, so
any caller in a recurring code path can flood the chat and get the bot
throttled by Telegram.

`chore/claude-baseline` fixed this at one call site (`653a1d2`, the rebalancer
loop) by making the notification edge-triggered. The other recurring callers
are currently safe only because they notify on a transition they then act on:
`launcher.js` restarts the service, `worker.js` kills the stuck process. That
is a convention holding by discipline, not by construction.

Consider a shared guard in `api/telegram.js` — suppress an identical message
within a window — so new call sites cannot reintroduce the problem.

---

## 5. `isLndAlive` logs before validating its argument

**Severity:** trivial
**Where it belongs:** opportunistic

`lnd-api/utils.js:132` logs `'lnd alive check'` before the
`if (!lndClient) throw` guard on the next line, so a bad call logs a check that
never happened.

---

## 6. os stats monitoring is only half implemented

**Severity:** medium — a full disk silently goes unmonitored
**Where it belongs:** feature work, its own branch

`api/constants.js:115-119` defines three monitored categories:

```js
cat: { mem: 'mem', cpu: 'cpu', disk: 'disk' }
```

`checkStats()` in `api/os-stats.js` only ever raises issues for `mem`.
`osStats()` collects `stats.cpu`, `stats.diskGb` and `stats.freeGb` and then
discards them, so the `cpu` and `disk` categories are declared but dead.

Disk is the one that matters: an lnd node that fills its disk stops working, and
the constants suggest this alert was meant to exist. Needs thresholds chosen
for both, in the shape the `mem` checks already use.

---

## 7. os stats alert text reports the measured value as the threshold

**Severity:** low — user-facing wording
**Where it belongs:** with item 6

`api/os-stats.js` builds messages as:

```js
msg: '[WARNING] memory utilization exceeds ' + stats.mem + ' %'
```

so at 86% utilisation the telegram alert reads "memory utilization exceeds
86 %", which states the measured value as if it were the threshold. The
threshold is 85. Running `node test/os-stats` reproduces it directly.

Should read either "is 86 %" or "exceeds 85 %".

---

## 8. The lint gate hides every warning

**Severity:** low — the gate is weaker than it looks
**Where it belongs:** with any lint tidy-up

`npm run check` runs `lint:errors`, which is `eslint --quiet`, so only errors
fail it. The config sets `no-unused-vars` and `eqeqeq` to `warn`, so those never
block. The tree currently has **17 warnings, 0 errors** — `npm run check` passes
and reports nothing.

That is a reasonable gradual-adoption stance, but "check passes" currently
means less than it appears. Either clear the 17 and promote the rules to
`error`, or leave them and be explicit that the gate only catches hard errors.

Also `eslint.config.cjs` ignores `old-jet-caporal.js`, which does not exist —
the real file is `jet.caporal.backup`, already covered by the `*.backup` entry.
Stale, safe to drop.

---

## 9. Typo in a peer-classification warning

**Severity:** trivial
**Where it belongs:** opportunistic

`api/utils.js:219` — `'cound find peer record for'` should be
`'could not find peer record for'`.

---

## 10. `tools/genconfig` still writes the pre-umbrel-0.5 macaroon paths — RESOLVED

**Resolved** for 1.7.0: `tools/genconfig` now writes the same `app-data/lightning`
paths as `docker/genconfig.sh` and the README.

**Severity:** high — every fresh host install starts broken
**Where it belongs:** its own branch, small

The umbrel path migration updated two of the three places that carry these
paths and missed the third:

| File | Path | Install route |
|---|---|---|
| `README.md` | `app-data/lightning/...` (new) | docs |
| `docker/genconfig.sh` | `app-data/lightning/...` (new) | Docker |
| **`tools/genconfig`** | **`/home/umbrel/umbrel/lnd/...` (old)** | **host** |

`tools/genconfig` is the host install path — it runs on `postinstall` and
generates `api/config.json`. So on umbrel 0.5+ a fresh host install writes a
config pointing at a macaroon that does not exist, while the README beside it
gives the correct path. The host route is the primary one, per the install
instructions.

Fix is to bring the two paths in `tools/genconfig` in line with
`docker/genconfig.sh`. Worth checking at the same time whether the two
generators should share a single source rather than duplicating the template.

---

## 11. Upgrade node-telegram-bot-api to 2.x to clear the last dependency criticals

**Severity:** medium — security debt
**Where it belongs:** its own branch, needs testing against a live bot

After `chore/runtime-modernization`, three critical advisories remain:
`form-data`, `request` and `tar`. The first two both come from
`@cypress/request`, which `node-telegram-bot-api@0.67.0` still depends on.
Bumping 0.66 to 0.67 does not move them — audited before and after, the result
is identical.

`node-telegram-bot-api@2.1.0` has **no dependencies at all**, so it drops the
chain entirely and would take criticals from 3 to 1, leaving only `tar` (via
`@mapbox/node-pre-gyp`, under `sqlite3`). It requires `node >= 18`, which the
new Node 22 baseline satisfies.

The catch is that 0.67 to 2.1.0 is a major version jump on the code path that
delivers every alert. The surface in use is small and contained - all of it in
`service/telegram.js`:

- `new TelegramBot(token, { polling: true })`
- `bot.onText(regex, handler)` x2
- `bot.sendMessage(chatId, msg)` x4, one with `{ parse_mode: 'HTML' }`

So the migration is likely small, but it must be verified against a live bot
before shipping: a silent break here means losing every notification, including
the ones that report that something else broke.

---

## 12. Retire `check:cli`, now subsumed by the help snapshot

**Severity:** low — redundant work, and a coverage gap while it stays
**Where it belongs:** opportunistic, small

`check:cli` is a hardcoded chain of `node jet <command> --help` calls. It lists
**22 of the 25 commands** — `start`, `stop` and `restart` were never added,
which is the drift a hardcoded list invites.

`test/cli-help-snapshot` now runs `--help` for every command *and* asserts the
output, and it discovers commands from the help output rather than a list, so
new commands are covered the moment they are registered. That makes `check:cli`
strictly redundant: it does less, on fewer commands, and both run on every
`npm run check`.

Left in place for now because removing it is a change to existing tooling
rather than part of adding the test. When retiring it, drop the script from
`package.json`, take it out of the `check` chain, and update the command list
in `CLAUDE.md`.

---

## 13. `describegraph.json` is not gitignored — RESOLVED

**Severity:** trivial
**Where it belongs:** opportunistic, one line

`bos` writes a `describegraph.json` into the repo root when certain commands
run. It is not in `.gitignore`, so it shows up as untracked in `git status` and
is easy to commit by accident.

Not produced by the new CLI surface tests — those leave the tree clean. Adding
`describegraph.json` to `.gitignore` is the whole fix.

**Resolved** for 1.7.0. The same change ignores the `jet-snapshot.db` and
`jet-recovered.db` copies pulled from the node for analysis.

---

## 14. Persist hop-level skips across rebalance runs

**Severity:** medium — wasted probes and coarse exclusions
**Where it belongs:** Jet 2.0

`skipHop` and `isSkippedHop` (`api/rebalance.js:590`) record the directed edges
jet learns to avoid, `node → next_node`, passed to bos as
`--avoid "FEE_RATE>N/next_node"`. They live in memory and are discarded when the
run ends, while the cruder whole-node exclusions persist in `rebalance_avoid`.
That is backwards: the edge is the more precise fact.

Fees are set per channel and direction. On the umbrel node MasterYoda charged
47 ppm on some routes and 1,218 on others for the same first hop, so excluding a
whole node to avoid one expensive edge discards its cheap ones. That is also why
1.7.0 widens the avoid lookup across budgets (`max_ppm >= budget`) but not across
routes.

The observed per-node fee is already recorded: `liquidity` holds it for 88 of 90
avoid entries, written by the same run. Edge persistence needs the edge, not a
new fee column.

Related: aggressive mode (`maxRuntime < 60`, which the default `maxTime: 30`
always selects) writes a within-run speed heuristic to the table, where it is
reused for 150 minutes. Persisted edges should record whether an entry was
speculative.

Kept out of 1.x because it changes exclusion behavior, and Jet 1.x is one of the
baselines Lightning Foundry's M3 measures.

---

## 15. Separate read and write LND credentials

**Severity:** required for managed mode; none standalone
**Where it belongs:** Jet 2.0

`api/connect.js`, `api/router-rpc.js` and `bos/connect.js` all load the one
`config.macaroonPath`. Lightning Foundry's managed mode needs two: a write
macaroon carrying a custom caveat, so that LND hands every payment and fee change
to Foundry's Policy before running it, and a read-only macaroon without the
caveat, so reads skip the middleware. bos executes the payments, so its handle
takes the write macaroon. With one path configured, jet keeps today's standalone
behavior.

See `lightningfoundry/docs/integrations/lightning-jet.md`.

---

## 16. Meet Lightning Foundry's invariants 1, 3 and 4

**Severity:** gates Jet 2.0 running as part of a Foundry node
**Where it belongs:** Jet 2.0

Invariant 1 requires pinned, hashed dependencies with no install scripts; 3, no
network path beyond LND; 4, verified release artifacts. Measured on `dev` on
2026-10-04, with dev dependencies included in the tree counts:

- **Dependencies float.** 11 of 13 direct dependencies use `^` or `~`.
- **Four packages run install scripts, not two.** `deasync` (`node ./build.js`),
  `sqlite3` (`prebuild-install -r napi || node-gyp rebuild`), `protobufjs`
  (postinstall, pulled in by `@grpc/proto-loader`), and jet's own
  `postinstall: ./tools/genconfig`.
- **balanceofsatoshis is the largest item, and the Foundry spec does not list
  it.** Its closure is 206 of 437 installed packages (47%), and 160 (37%) are
  there only because of it. It executes every rebalance, so removing it means
  driving LND's router RPC directly: route queries, probing, `SendToRouteV2`. Jet
  already has a router client in `api/router-rpc.js`.
- **sqlite3 can be removed.** `node:sqlite` works here on Node 23.10 without a
  flag and should on the node's 22.23, but verify there; it is still marked
  experimental. Its `DatabaseSync` is synchronous, so the db layer would no longer
  need `deasync` either.
- **deasync** then remains only in the `lnd-api/utils.js` gRPC wrappers;
  converting those to async removes it.
- **Telegram** is the only outbound network path in jet's own code; the other
  URLs are help text. It becomes a separate notifier that holds no credential,
  which the spec allows. bos needs its own invariant-3 audit.
- **genconfig** becomes an explicit `jet init` rather than an install hook.
- **Releases** publish versions, hashes and signatures.

---

## 17. No ceiling on total rebalance fees per day — RESOLVED

**Resolved** for 1.7.0 with `rebalancer.maxDailyFee`, default 100,000 sats per
rolling 24 hours, 0 to disable. The rebalancer checks it before spawning each
automated rebalance, counting fees paid (manual included) plus the full fee limit
of runs still within their time limit; `db.rebalanceFeesCommittedSync` does the
accounting. On the umbrel node's history the default would have bound on 5 of 549
days with rebalancing; the busiest day spent 200,344 sats.

**Severity:** high — an unbounded loop that spends money
**Where it belongs:** 1.7.0

The only fee bound is per rebalance: `maxFee = amount × ppm / 1e6`
(`api/rebalance.js:97`). The rebalancer loops every two minutes with up to
`maxInstances` runs at once, and nothing caps what it spends in a day.

Lightning Foundry's threat model relies on that bound. A peer that can predict
rebalances can provoke them and collect the fees, and in standalone mode "Jet's
own limit is the only bound". Today there is no such limit. Add a deterministic
daily ceiling, checked before each rebalance starts against fees already paid per
`rebalance_history`, with a default high enough not to bind in normal operation.

---

## 18. Child processes find `node` on PATH — RESOLVED

**Resolved** for 1.7.0: both spawn sites use `process.execPath`.

**Severity:** medium — latent crash since the Node 22 migration
**Where it belongs:** 1.7.0

`service/utils.js:43` starts every daemon with `cmd: 'node'`, and
`service/rebalancer.js:562` executes the `jet` script, whose
`#!/usr/bin/env node` shebang also searches PATH. Started from a context where nvm
is not initialized, such as cron, systemd or non-interactive SSH, children get the
system Node (`/usr/bin/node` v16 on the umbrel host) and crash loading native
modules built for 22. Use `process.execPath` in both places so children run on the
parent's binary.

---

## 19. Rebalance avoid-list messages and retries

**Severity:** low
**Where it belongs:** 1.7.1. Cut from 1.7.0 to keep that release small.

Found while measuring rebalances on the umbrel node in September 2026:

- **Misleading message.** When the only route is two hops between the run's own
  endpoints (MasterYoda → Kraken at 1,048 ppm), `canAvoidNode` rightly refuses to
  exclude either, and jet logs "couldnt exclude any nodes, likely already on the
  avoid list". Nothing was on the avoid list; the route had no excludable node.
- **Futile retry.** In that case the next probe is guaranteed identical, so jet
  should stop instead of retrying. A failed probe spends nothing, so this costs
  only time.

Silent failures when inserting avoid entries are item 3.
