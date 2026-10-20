# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Lightning Jet ("jet") is an automated channel rebalancer for LND nodes on the Lightning Network. It ships as a CLI (`jet`) plus a set of long-running background daemons that classify peers by routing volume, run circular rebalances, analyze fees/HTLCs, and notify via Telegram. CommonJS Node.js project, no build step.

## Common commands

```bash
node jet <command> --help   # explore any command; ./jet is the installed bin (adds itself to PATH)
npm run lint                 # eslint over jet, cli/**, service/**
npm run lint:errors          # eslint --quiet (errors only)
npm run check:cli            # smoke-test: runs --help for every registered command
npm run test:cli-surface     # golden-file snapshot of the CLI surface + README flag checks
npm run check                # check:cli + test:cli-surface + lint:errors — run before considering CLI work done
```

There is **no test framework**. Files under `test/` are standalone scripts run directly (`node test/fee-monitor.js`); many set `global.testModeOn = true` and/or `global.testDb` to avoid touching live LND/db. Some `test/` entries are directories of such scripts.

`test/cli-help-snapshot` compares `--help` for every command against the committed `test/cli-help.snapshot`; after an intentional CLI change, regenerate with `node test/cli-help-snapshot --update` and review the diff. `test/cli-documented-flags` runs the invocations the README promises and fails only on a Commander parse error, so it works with or without a live LND. It invokes only commands that cannot take an action — never `rebalance`, `pay`, `send-message`, `update-channel`, `close-channel` or `start`/`stop`/`restart`; flags reachable only through those are covered by the snapshot instead.

Runtime config lives in `api/config.json` (git-ignored, created from a template by `tools/genconfig` on `postinstall`). It needs valid `macaroonPath`/`tlsCertPath` to reach a real LND node, so most commands and all services fail without a configured node — expect this in a dev checkout and don't treat it as a code bug.

## Architecture

Layered, with a hard split between the user-facing CLI and the always-on daemons:

- **`cli/`** — Commander-based CLI. `cli/index.js` builds the `program` and calls a `register<Name>Command(program)` exported from each `cli/commands/*.js`. Adding a command = new file exporting a register function + one line in `index.js`. `cli/utils/lnd.js` wraps LND access (`requireLndAlive()`).
- **`api/`** — core logic and the primary LND gRPC client. `api/connect.js` = `lnrpc` (Lightning) client, `api/router-rpc.js` = Router client, both built from proto files in `api/proto` via `lnd-api/connect.js`. Also houses rebalance, htlc-analyzer, analyze-fees, telegram, os-stats, and `api/constants.js` (all tunable defaults; `config.json` overrides them).
- **`lnd-api/`** — low-level gRPC helpers and the `*Sync` wrappers used everywhere. `lnd-api/utils.js` exposes synchronous versions of async gRPC calls.
- **`bos/`** — balanceofsatoshis integration for the actual rebalance/pay/reconnect execution. **Uses its own LND handle** via `ln-service` (`bos/connect.js`) — it cannot reuse `api/connect.js`'s handle. Does not require the `bos` binary to be installed.
- **`db/`** — sqlite (`db/jet.db`). `db/utils.js` is the data layer; table list and all accessors live there.
- **`service/`** — the daemons. `service/utils.js` defines a `Service` base class and subclasses whose `static name` is the CLI-visible service name: `daddy` (Launcher — the watchdog), `rebalancer`, `logger` (HtlcLogger), `worker`, `telegram` (TelegramBot). Managed with `jet start|stop|restart <name>` (or `all`) and `jet status`.

### Daemon model

`daddy` (the Launcher, `service/launcher.js`) is the watchdog: it starts the other services, watches their heartbeats (stored in the db `nameval` table via `recordHeartbeat`/`lastHeartbeat`), and restarts any that go stale. `service/rebalancer.js` enforces a single instance via `isRunningSync`. Each service logs to its own file: `/tmp/jet-<service>.log`; individual rebalances log to `/tmp/rebalance_<from>_<to>.log`.

## Conventions and gotchas

- **`deasync`-based sync wrappers**: the `*Sync` functions in `db/utils.js` and `lnd-api/utils.js` block the event loop to return synchronously. This is the dominant style — match it in existing sync code paths rather than converting to async.
- **`import-lazy`**: services and some commands require `api/config`, `api/connect`, and `lnd-api/utils` lazily (`importLazy('../api/config')`) so that code paths not needing LND don't crash on a missing/invalid `config.json`. Preserve this — don't hoist those requires to the top eagerly.
- **ESLint scope is deliberately narrow**: `eslint.config.cjs` (flat config) lints only `jet`, `cli/**`, and `service/**`. The `api/`, `lnd-api/`, `bos/`, and `db/` layers are older code and are intentionally not linted; don't assume lint covers a change there.
- **CLI was migrated from caporal to Commander.** `jet.caporal.backup` (untracked) is the old CLI kept for reference; `before-*.txt` are pre-migration `--help` snapshots used to check output parity. These are not part of the running code.
- Config resolution differs by install: host installs read `api/config.json`; the Docker path uses `$HOME/.lightning-jet/config.json` (`docker/genconfig.sh`).
- **Native module builds are pinned in `package.json`.** `overrides` forces `node-gyp` to 11.x because the 8.x that `sqlite3` pulls in imports Python's `distutils`, which Python 3.12+ no longer ships; deasync has no prebuilt binaries for Node 22+, so every install compiles it. `allowScripts` approves the install scripts of `deasync` and `sqlite3` by exact version (npm says a future release will block unapproved ones). After bumping either package, run `npm approve-scripts <pkg>` or its build will be skipped once npm enforces this.
