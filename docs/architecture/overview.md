# Jet 2.0 architecture

Lightning Jet 2.0 is a ground-up rebuild of Jet, an autonomous economic engine for
LND routing nodes. This document is the design it is built to: what it keeps
from 1.7, what it must satisfy, and the shape of its first scope, automated fee
management and circular rebalancing.

The records and rules every decision follows are in
[`decision-contracts.md`](decision-contracts.md). How 1.7 works, read file by
file, is in [`../reference/jet-1.7-internals.md`](../reference/jet-1.7-internals.md).

## Objective

Maximize the incremental net value of the node's liquidity: routing revenue the
node earns because of what Jet did, minus everything Jet spent. A successful
rebalance is not a goal, and neither is a fee change. Jet decides fees and refills
from one per-channel model of what liquidity earns and what it costs, inside
deterministic limits, and records every decision so it can be explained and
evaluated.

A rebalancer looks for routes. Jet 2.0 decides whether looking for one is worth
it.

## What 1.7 teaches

1.7 rebalances by trying many routes and keeping the few that fit a budget, and
leaves fees to the operator.

**Signals worth keeping:**

- Routing volume per channel, read from LND's forwarding history.
- Forwards that failed for lack of balance. These hint at missed demand but don't
  prove it: they include retries of the same payment through other channels,
  oversized HTLCs, and probes, which look the same as payments.
- The measured cost of each refill, per channel, from LND's payments.
- Hop prices discovered while probing routes.

**Ideas worth keeping.** 1.7's rebalancer was hand-built and novel in several
ways, and most of its ideas carry into 2.0, rebuilt on per-channel economics.
The full list is in the
[1.7 reference](../reference/jet-1.7-internals.md#13-design-ideas-worth-keeping).

- Liquidity sized to each channel's observed flow, not a fixed half.
- A portfolio of strategies (regular, missed demand, just-in-time
  replenishment, idle-channel revival) sharing one slot limit, with quotas so no
  single signal takes over.
- Guards before acting: pending HTLCs, fee anomalies, in-flight accounting,
  one run per pair, exponential backoff, and a daily ceiling that fails closed.
- A route search that explores under a budget. It excludes the costliest hop,
  is more patient when it has more time, and remembers exclusions per pair and
  per budget, with expiry.
- Every probed hop recorded as a liquidity sighting, and the cheapest failed
  price kept as the price that would have worked.

**Design problems not to repeat:**

- **Units.** Channels are classified one by one, but every rebalance names two
  peers and the payment tool picks the channels.
- **Budget.** A refill's budget is the target channel's own fee. With
  profitability enforcement off, the default, it's raised above that, and the
  configured maximum ppm is not a cap unless enforced. Jet can pay more to refill
  a channel than the channel earns.
- **Reflexes.** A forward over a threshold immediately queues a refill of the same
  amount, reversing the flow that just earned a fee.
- **Learning.** The cheapest failed route is recorded and shown to the operator,
  but never fed back into decisions. Pairs that
  can't succeed retry forever, and timeouts leave no record at all. Per-hop fee
  exclusions are built but never reach the payment tool.
- **Fees.** Advisory only, keyed by peer rather than channel, unaware of inbound
  fees. The one manual write path reports success even when LND rejects the
  update.
- **Runtime.** One OS process per rebalance. Blocking calls on the event loop.
  Text parsing of the payment tool's output. No deadlines on RPCs. Services found
  by matching process names. The admin macaroon for everything.
- **Data.** SQL built by string concatenation, no schema versions, no indexes on
  the dates queries filter by, and a cleanup that locks the whole database while
  it vacuums.
- **Notifications.** Anyone who sends the Telegram bot `/start` or `/help`
  becomes the recipient of every alert, replacing the operator.

A gRPC upgrade in 1.7.0 broke the HTLC logger only once it subscribed to a live
LND, and no test exercised that. So every 2.0 service is tested against a real
LND before release.

## What Lightning Foundry requires

[Lightning Foundry](https://github.com/OogaBoogaX/lightningfoundry) is the
platform that runs a node's stack. Its
[ADR 0011](https://github.com/OogaBoogaX/lightningfoundry/blob/rock/docs/decisions/0011-jet-optimizes-foundry-operates.md)
says Jet decides and acts, while Foundry operates and authorizes. In managed
mode:

1. Jet sends an intent, meaning an action with its bounds.
2. Foundry's Policy approves it, either immediately or after asking the operator.
3. Jet makes the LND call itself, with a macaroon that carries a custom caveat.
4. LND hands each such call to Policy, registered as RPC middleware, which lets
   it through only if it matches an approved intent. The check fails closed,
   and Jet's reads go through a separate read-only macaroon.

This shapes 2.0 throughout:

- **Dependencies** are pinned, hashed, justified, and free of install scripts.
  The standard library comes first.
- **No outbound network from the core.** Notifications run in a separate process
  that holds no LND credential.
- **Models propose; deterministic code decides** every limit.
- **Reading and acting use separate credentials, never the admin macaroon.** On a
  node Foundry adopts rather than installs, that also means Jet runs as an OS
  user, or in a container, that cannot read, copy or replace LND's privileged
  credentials. This is enforced and tested at deployment, and is a prerequisite
  for managed live execution.
- **Every autonomous decision is recorded, evaluated and attributable.**
- **Jet drives LND directly.** Policy can only match calls it recognizes, so Jet
  publishes the exact list of LND calls it makes.

Managed mode means one local node running under Foundry's Policy. Foundry rules
out a hosted control plane.

## The decision loop

```
observe ──► model ──► decide ──┬──► intent ──► authorize ──┬──► execute ──► record
                               │                           └──► rejected ──► record
                               └──► abstain ───────────────────────────────► record

record ──► learn ──► model
```

The loop is the same standalone and managed. Standalone, Jet's own deterministic
limits authorize. Managed, Foundry's Policy authorizes, and LND checks each call
against the approved intent. Every evaluation ends in an intent or a recorded
abstention ([contracts](decision-contracts.md)).

## Economics model

Fees and rebalancing share it.

- **Signals stay apart, each with its own confidence.** Completed forwards are the
  baseline. Insufficient-balance failures are an opportunity indicator. They are
  stored with size, time, both channels and failure type, deduplicated across
  channels, and never a reason to refill on their own. The other signals are
  balances, our and peers' policies over time, and refill outcomes, which lose
  weight with age.
- **Replacement cost** per channel is the median ppm of recent successful refills
  into it. Without any, it is the higher of the peer's fee plus a buffer and the
  cheapest failed route.
- **Marginal value of outbound liquidity** per channel is computed at different
  balances, over a stated horizon. It is the *incremental* routing revenue the
  liquidity enables compared with doing nothing, counting reuse when flow is
  two-way (liquidity that comes back routes again), minus the cost of draining
  the source and an allowance for uncertainty. One component computes it, and
  fees, rebalancing and later channel allocation all use it.
- **Low confidence means no action.** Where the evidence can't support an
  estimate, the engines hold and say why. Data quality and economic confidence
  are reported separately.
- **Two budgets.** An operating budget, funded by realized routing revenue and
  accumulating over weeks. And a small exploration budget, set by the operator,
  for promising channels without revenue history, with a stopping rule per
  channel. Both sit under absolute daily and monthly loss ceilings. A strict share
  of trailing revenue doesn't work on its own: on a node earning 40 sats a day, a
  50% share would take three weeks to fund a single 400-sat refill.

## Fee engine

- **A deterministic controller.** It raises fees on drained channels with demand
  and lowers them toward the floor on full, idle ones, acting only outside
  balance bands (for example, below 20% or above 80% local).
- **Evidence before change.** Each channel has a baseline policy. A change needs
  a stated hypothesis and enough evidence. It runs for an observation window long
  enough to tell a result from noise, and rolls back if things get worse. On a
  channel with a handful of forwards a month, the usual answer is no change.
- **Price evidence beyond our own traffic.** The local gossip graph shows what
  competing routes advertise, without any outbound call. It is a prior that our
  own results correct, not the market price: it shows neither where liquidity
  sits nor which paths senders choose. The peer's fee toward us and
  insufficient-balance failures add evidence; the failures show senders accepted
  our current price.
- **Steps and caps.** Multiplicative steps of 5 to 15%, a cooldown of 6 to 24
  hours per channel, and caps per channel and per node, to respect gossip limits
  and avoid fee wars.
- **A conditional floor.** The fee floor sits at replacement cost plus margin, but
  only on channels Jet intends to refill. Jet may conclude a channel shouldn't be
  refilled at all.
- **Experiments, not shadow alone.** Shadow mode can't show what a price change
  would have done. Live changes are small, bounded experiments with evaluation
  windows set in advance.
- **Safe writes.** Policy writes carry over CLTV and HTLC limits, check
  `failed_updates`, and read the policy back. Inbound fees, as discounts only,
  come later.

## Rebalancing engine

- **Refill only when the value is positive, with a margin.** Targets whose market
  cost exceeds their value stop being pursued, rather than being retried with
  backoff forever.
- **LND's router RPC directly,** with fee-limited route queries and mission
  control. Jet learns costs per edge and per channel pair, with expiry.
- **A durable job table** with liquidity reservations per channel. Every attempt
  is recorded once: job, channels, route, fee in msat, outcome, duration.
- **Payment safety, with two identities.** The *intent id* carries the
  authorization and the budget reservation, so an action can't execute twice.
  - Each *attempt* under it starts from a self-invoice, and that payment hash is
    recorded before the send.
  - LND won't let a hash succeed twice or run two attempts at once, but it does
    allow a retry after a failure. And a fresh invoice is a fresh hash, so the
    hash alone can't stop a duplicate.
  - After a crash or a timeout, Jet recovers the intent and asks LND for the
    status of its attempts (`TrackPaymentV2`) before doing anything.
  - A new attempt opens only after the previous one has definitely failed, within
    the intent's authorization and budget.
  - A timeout never counts as a failure.
- **Channel identity.**
  - `SendPaymentV2` pins the first hop (`outgoing_chan_ids`), but only the last
    hop's *peer* (`last_hop_pubkey`), and a peer may forward over any of its
    channels to us.
  - Accounting reads each HTLC's arrival channel from the settled invoice.
  - The first release sends single-part refills only (`max_parts = 1`).
  - Avoiding nodes needs `QueryRoutes` with ignore lists plus `SendToRouteV2`,
    since `SendPaymentV2` has no ignore list.
- **Low-volume revival is opt-in,** with its own small budget.

A payment execution specification covers all of this before the engine makes
any live payment.

## Limits

Every intent passes one deterministic module, in both modes. It enforces:

- daily and monthly loss ceilings;
- the operating and exploration budgets, with reservations made before
  execution;
- minimum and maximum ppm and base fee;
- a maximum number of fee changes per channel per day;
- a freeze list (channels Jet must not change) and a never-list (peers no refill
  routes through);
- a kill switch;
- a mode per engine: `off`, `shadow` or `live`.

Shadow is the default. An engine graduates to live on evidence, meaning enough
decisions checked against what actually happened, not on elapsed time.

## Operations baseline

2.0 replaces 1.7 without losing what operators rely on:

- health checks;
- peer reconnects;
- channel and database alerts;
- notifications.

Notifications come from a separate notifier process with an allowlist of
recipients and no LND credential.

## Running Jet

**One process.** `jet run` runs everything in a single foreground process:
observation, the engines and the LND subscriptions. It logs to standard output
and standard error, answers a health check, and stops cleanly on `SIGTERM` or
`SIGINT`. Work interrupted by a stop is reconciled with LND on the next start,
never assumed finished or failed. Jet never daemonizes or supervises itself.
1.7's watchdog (`daddy`), its process-name matching and its process per rebalance
are gone.

**The platform supervises it:**

| Where | Supervisor | Notes |
|---|---|---|
| Linux host | systemd | A hardened unit: Jet's own OS user, which can't read LND's admin macaroon; a read-only file system apart from Jet's data directory; network restricted to LND where systemd's address filters are available. This enforces Foundry's isolation and no-outbound-network rules at the OS level. |
| App platforms (Umbrel, Start9) | the container runtime | A container image, networked to LND only |
| Foundry managed mode | Foundry's supervisor | Jet only has to be a well-behaved foreground process |
| macOS, for development | launchd, or run directly | |

**Talking to Jet:**

- **The CLI,** from the first slice: `jet init`, `jet run`, `jet status`,
  `jet explain <channel>` and `jet report`. Later come `jet mode <engine>
  off|shadow|live` and `jet pause` / `jet resume`.
  - Read-only commands read the database directly; WAL mode allows readers
    alongside the daemon.
  - Commands that change state go to the running daemon over a local Unix
    socket protected by file permissions, so Jet opens no network port.
- **A local web view,** after the observe phase, read-only at first: channels,
  decisions and their explanations, and budgets.
  - HTML is generated by the Go server and built into the binary, with no
    JavaScript toolchain.
  - It listens locally only, or behind the app platform's authentication.
  - Controls come later, the pause switch first, and need authentication and
    protection against cross-site request forgery.
- **Notifications** come from the separate notifier, which is never a control
  channel. Nothing reachable from the internet can make Jet act.
- **In managed mode,** Foundry may show Jet's decisions in its own interface.
  `jet explain --json` and the records behind it are the stable feed for that.

**Order:**

1. The slice ships the CLI only.
2. The observe phase adds a hardened systemd unit on the low-volume node, running
   Jet as its own user with a read-only macaroon. That also tests the OS-level
   isolation managed mode requires.
3. Then the web view and a container image for app platforms.
4. Then controls.

How early the web view and container come depends on two open questions,
listed under [Decisions](#decisions).

## AI-native

- **From the first release.** Strategies are versioned and swappable. Every intent
  and abstention carries its rationale and the inputs behind it, and holdouts and
  shadow mode exist. That is the plumbing a model needs to earn trust.
- **Explanations from the first release.** `jet explain` answers why Jet did or
  didn't act, from recorded data.
- **Deterministic strategies first.** Learned models come later: small, trained on
  the node's own history, and run on the node. They arrive once there is enough
  logged history to evaluate them against the deterministic baseline.
- **Language models advise and never decide.** They can explain decisions,
  summarize, propose parameter changes and label peers, in a separate, opt-in
  process. An LLM deciding fees or rebalances each cycle conflicts with Foundry's
  invariants.

## Data model, sketch

| Table | Holds |
|---|---|
| `channels`, `peers` | Channel ids, capacity, peer, open and close dates |
| `balance_snapshots` | Local and remote balance per channel, hourly |
| `policies` | Our and peers' fees over time (base, ppm, inbound, CLTV, HTLC limits, disabled), with source |
| `forwards`, `htlc_failures` | From LND, at msat precision, all failure types |
| `snapshots`, `estimates`, `intents`, `abstentions`, `decisions`, `outcomes` | The decision records |
| `jobs`, `attempts` | Durable execution work, one row per attempt |
| `cost_estimates` | Learned replacement cost per channel and per edge, with confidence and freshness |
| `meta` | Schema version |

Migrations are versioned. The database runs in WAL mode, uses bound parameters
only, and has a retention policy per table. Observations, estimates and decisions
never share a table.

## Configuration, sketch

```toml
[lnd]
address       = "localhost:10009"
tls_cert      = "/path/tls.cert"
read_macaroon = "/path/readonly.macaroon"
act_macaroon  = "/path/jet-act.macaroon"   # never the admin macaroon

[limits]
daily_loss_ceiling_sat           = 10000
monthly_loss_ceiling_sat         = 50000
operating_budget_share           = 0.5     # of realized routing revenue
operating_budget_window_days     = 30      # unspent allowance carries over
exploration_budget_sat_per_month = 5000
fee_ppm_min                      = 1
fee_ppm_max                      = 3000
max_fee_changes_per_channel_per_day = 2
freeze = ["<channel id>"]                  # channels Jet must not change
never  = ["<peer pubkey>"]                 # peers no refill routes through

[fees]
mode = "shadow"            # off | shadow | live

[rebalance]
mode = "shadow"
low_volume_revival = false

[notify.telegram]          # separate process, no LND access
token_file       = "/path/telegram.token"
allowed_chat_ids = [123456789]
```

Keys carry their units, and values are validated at start. `jet init` writes the
file; there is no install script.

## Code layout

```
cmd/jet/            CLI and daemon
internal/
  contracts/        snapshot, estimate, intent, abstention and decision records
  economics/        estimators: demand, replacement cost, value of liquidity
  strategy/         fee and rebalance strategies
  limits/           deterministic authorization
  engine/           scheduler and the decision pipeline
  explain/          renders recorded decisions
  lnd/              generated LND client code, read and act clients, a fake LND
  store/            SQLite schema, migrations, queries
  control/          the local socket the CLI uses to reach the daemon
  web/              the local web view (later)
  notify/           the separate notifier process
docs/
testdata/           synthetic fixtures only
```

## Runtime

Go:

- a static binary per platform;
- predictable concurrency, with deadlines and cancellation on every LND call;
- native 64-bit channel ids, and distinct types for sat and msat amounts;
- the language of the LND ecosystem.

The module is `github.com/drneski/lightning-jet/v2`. SQLite comes from
`modernc.org/sqlite`, pure Go, so builds need no C toolchain. LND client code is
generated from pinned `.proto` files rather than importing LND's own Go
packages, which would bring in all of LND.

## Testing and evaluation

- **Unit tests** on the pure core, and a fake LND for the engine and adapters.
- **Regtest** for payment execution, including crashes mid-payment and restarts.
- **Replay** of recorded histories through the core. The repository holds
  synthetic histories only. Replay against real node data happens locally,
  outside the repository.
- **A real LND for every service** before each release.
- **Two real environments.** A low-volume node is where doing nothing has to be
  recognized as rational. A high-volume node is where strategies can show they
  earn more. On a quiet node a good controller may rightly do almost nothing,
  which proves it avoids bad actions but not that it earns. The observer runs on
  the high-volume node as early as possible, so there is history by the time the
  engines go live.
- **Comparison against simple alternatives:** doing nothing, and static rules.
  Success is measured as:
  - net routing revenue after costs;
  - liquidity utilization;
  - refill efficiency;
  - preventable failures;
  - operational reliability;
  - the share of decisions that can be explained after the fact.

  Fewer attempts is not a goal in itself.

## Migration from 1.7

**Preserve everything, import selectively.** The 1.7 database is copied whole
and kept, untouched and read-only, beside 2.0's own, so nothing is lost and the
importer can be re-run later against the copy. Into 2.0's schema goes only what
2.0 can trust, labeled with its source (`jet-1.7`). 1.7's tables mix grains,
round fees to whole sats and mostly lack channel ids; copied verbatim, they would
contaminate 2.0's estimates.

| 1.7 table | What happens | Why |
|---|---|---|
| `txn` (forwards, rebalance payments) | **Rebuilt from LND**, at msat precision. 1.7 rows fill only gaps LND no longer has, such as pruned payments, marked as whole-sat. | LND is the source of truth; `txn` is a 60-day, whole-sat copy of it |
| `failed_htlc` | **Imported** (what's left of 60 days) | LND keeps no history of failed forwards. Marked as having no payment hash and only one failure type, so estimators weight it accordingly |
| `channel_events` | **Imported** in full | LND keeps no history of channels going up and down |
| `fee_history` | **Imported** in full, as peer policy observations | LND keeps only current policies. 1.7 skipped zero fees, so the gaps are marked |
| `rebalance_history` | **Successes imported** in full as history. **Failures summarized** by pair, reason and month. | Mostly years old, with peers rather than channels, so it never seeds cost estimates. Recent refills come from LND's payments, which carry the channels |
| `liquidity`, `rebalance_avoid` | Left in the archive | Short-lived and node-level, where 2.0 learns per edge |
| `active_rebalance` | Must be empty at cutover | |
| `nameval`, `nameval_list`, `telegram_messages` | Left in the archive, except the Telegram chat id, which is re-confirmed against the new allowlist | Heartbeats, cursors, throttles and queues |

Two more things carry over:

- **Configuration:** LND connection settings, limits and exclude lists, mapped
  with 1.7's default rules.
- **The last 24 hours of spend,** rebuilt from LND's payments, so the fee ceiling
  carries straight over.

The archive and everything imported are the node's private data. They stay in
the node's local data directory.

**Cutover:**

1. Copy 1.7's database with SQLite's backup, never reading it live, and import
   from the copy. Rehearse the import as a repeatable dry run, with checks that
   old and new records reconcile.
2. Run 2.0 in shadow next to 1.7, with different paths and no notifier token, so
   the two never collide or both act.
3. Disable 1.7's rebalancer and let running rebalances drain. Then switch 2.0's
   engines to live one at a time. 2.0 refuses to go live while 1.7's rebalancer
   runs.
4. Rollback: stop 2.0, and start 1.7 with its rebalancer disabled for a day,
   since 1.7 cannot see 2.0's recent spending.

## Repository, branches and releases

- **Branches before launch.** 2.0 is built on `v2`, an orphan branch that shares
  no history with 1.7. Topic branches come off `v2`. `main` and `dev` stay 1.7,
  untouched, and `main` stays the default branch.
- **At launch.** `main` is renamed `legacy/1.x`. Then `v2` is renamed `main` and
  made the default, and a new `dev` replaces 1.7's. Renaming rather than merging
  makes an old clone's `git pull` stop with an error, instead of silently
  swapping a running 1.7 install for 2.0 code. That is only defense in depth,
  though: the migration guide is the real instruction.
- **Tags** are plain semver (`v2.0.0`, with pre-releases like `v2.0.0-alpha.1`),
  and a release's theme goes in its title. Tags are never moved, because Go's
  checksum database records every version permanently.
- **Distribution.** The `jet` binary lives in `cmd/jet`:
  `go install github.com/drneski/lightning-jet/v2/cmd/jet@<version>`. Signed
  release binaries are the main way to install.

## Phases

1. **Design and contracts:** the decisions above, the
   [decision contracts](decision-contracts.md), and the test harness.
2. **Vertical slice, with no writes.** One end-to-end path:
   - connect to LND with read-only credentials;
   - estimate one channel's economic position, with explicit uncertainty;
   - propose an intent or abstain;
   - authorize it in shadow mode;
   - record the decision and explain it.
3. **Observe.** Widen the slice to every channel and build the per-channel
   economics report. Still read-only.
4. **Fees.** Shadow, then bounded live experiments.
5. **Rebalancing.** The payment execution specification, then shadow, then live
   within limits.
6. **Migration and cutover** from 1.7.
7. **Foundry managed mode.** The intent interface, and OS-level credential
   isolation.

**Prerequisites:**

- A high-volume evaluation node before phase 4 is judged.
- OS-level isolation from LND's privileged credentials before any managed live
  execution.

## Decisions

**Settled:**

- **Runtime:** Go.
- **Repository:** this one, keeping its name. 2.0 is built on `v2`, and 1.7 stays
  on `main` until launch.
- **AI-native:**
  - explainable decisions and versioned, model-ready strategies from the start;
  - deterministic limits and strategies first;
  - language models as advisors only.
- **Managed mode:** Foundry-operated nodes, one local node under Foundry's
  Policy.
- **Standalone approvals:** automatic within limits. No internet-connected bot is
  on the approval path.
- **Shadow graduation:** on evidence, not elapsed time.
- **1.7 as baseline:** it stays runnable and reproducible through migration, on
  its legacy branch and tags.
- **Telegram:** an isolated notifier with an allowlist and no credentials.
- **Running:** one foreground process supervised by the platform, and the CLI
  first. Nothing reachable from the internet controls Jet.
- **First scope:** fees and rebalancing, plus the operations baseline. Inbound
  fees, channel opening and closing, swaps and topology come later.

**Open:**

- How intents and verdicts map onto Foundry's schema in managed mode. To be
  drafted in Foundry alongside 2.0's core.
- The payment execution specification, due before phase 5.
- Who standalone 2.0 is mainly for. App-platform users would bring the web view
  and the container image earlier. Command-line node runners would make systemd
  and the CLI enough for longer.
- Whether OBL and Foundry will show Jet's decisions in their own interface. If
  they will, a stable data feed matters more than Jet's own web view.
