# Lightning Jet 1.7 internals

What the 1.7.0 code actually does, read file by file, as the reference for the
Jet 2.0 rebuild. It describes behavior, not intent: where the README, a comment
or a name says one thing and the code does another, this follows the code.
Line references are to the tag `v1.7.0-node22` (commit 67f770b), whose code
lives on `main` until 2.0 launches and on `legacy/1.x` after.

## 1. Processes

Five long-running services plus one short-lived process per rebalance. Each is a
separate Node process with its own LND connections, its own sqlite handles and
its own log file in `/tmp/jet-<name>.log`.

| Service | File | Loops | What it does to LND |
|---|---|---|---|
| `daddy` | service/launcher.js | 1 min | none |
| `rebalancer` | service/rebalancer.js | plan every 120 s, dispatch every 10 s | spawns rebalance processes, which pay |
| `logger` | service/htlc-logger.js | two LND streams, 20 s health check | none (subscribes only) |
| `worker` | service/worker.js | 60 s, 1 min, 5 min, 60 min, 24 h (below) | reconnects peers hourly |
| `telegram` | service/telegram.js | fees every 10 min, bot poll every 15 s | none |
| rebalance-proc | service/utils/rebalance-proc → api/rebalance.js | one run, then `process.exit()` | circular payments via bos |

**daddy** starts any service that isn't running, except the rebalancer when
`rebalancer.disabled` is set and telegram when there is no `telegramToken`. It
restarts a service whose heartbeat (a `nameval` row) is stale: rebalancer after
4 min, worker after 10 min, telegram after 20 min (fees) or 30 s (poll). It
restarts the logger when the logger sets the `htlcLoggerError` prop. Every start
and stop sends a Telegram message.

**Finding processes.** `isRunningSync` and friends match process command lines
across the whole host with `find-process`; `jet stop` kills the first match.
Services are started detached with `process.execPath`; rebalances are spawned
through `$SHELL` with output going to `/tmp/rebalance_<from>_<to>.log`.

**Blocking style.** Almost every LND and db call is made synchronous with
`deasync`. A call whose callback never fires blocks its loop forever; the
heartbeat then goes stale and daddy restarts the service. `htlcHistorySync`
does exactly that on any gRPC error (lnd-api/utils.js:216: it logs and never
sets the value it waits on).

## 2. LND access

- `api/connect.js` (Lightning), `api/ln-rpc.js` (Lightning again, used by the
  logger) and `api/router-rpc.js` (Router) are gRPC clients built from
  `api/proto` by `lnd-api/connect.js`: @grpc/grpc-js 1.14, proto-loader 0.7,
  protobufjs 7. The protos predate inbound fees.
- `bos/connect.js` opens a separate `ln-service` handle for balanceofsatoshis;
  it cannot share the gRPC clients.
- Credentials: one macaroon path (the admin macaroon in practice) and one TLS
  cert; `serverAddress` defaults to `localhost:10009`. No per-call deadlines.
- protobufjs 7 refuses to encode a missing request; every call must pass `{}`
  (this is the post-release logger fix, 67f770b).

## 3. The automated rebalancer

`service/rebalancer.js` plans and dispatches; `api/rebalance.js` (§4) performs
one run.

### 3.0 Startup

- **One instance only,** enforced by matching its own file name among the
  host's processes.
- **The exclude list is read once.** `rebalancer.exclude` entries are pubkeys,
  optionally with a type: `<pubkey>` or `<pubkey>:<type>`. The type is `all`,
  `inbound` (alias `from`), or `outbound` (alias `to`, and the default). So a
  bare pubkey only stops refills *into* that peer. Each pubkey is checked
  against the current peers, and unknown ones are skipped with a Telegram
  alert. Changing the list needs a restart.
- **The cycles.** The planning cycle runs at once and then every 120 s, recording
  a heartbeat each time, even when LND is down. Dispatch starts after 10 s and
  runs every 10 s. A loop error is sent to Telegram once, not on every repeat.

### 3.1 Classification (api/utils.js `classifyPeersSync`, 1 day)

Built from `forwardingHistory` for the last day, capped at 10,000 events with no
paging (lnd-api/utils.js:208). Volume is summed **per channel**: `chan_id_in`
for inbound, `chan_id_out` for outbound. Rebalance payments are not forwards, so
only real routing counts.

- `p` is a channel's share of all inbound (or outbound) volume. At `p ≥ 1%` a
  channel is **inbound** or **outbound**. If it is both, the larger side wins and
  `split` records that side's share of the channel's two-way volume.
- Everything else goes to **balanced** ("low volume"), or **skipped** when
  capacity is below `minCapacity` (990,000).
- The "channel younger than a week is balanced" rule never fires: it compares
  `now − lifetime` (a timestamp) with a duration (api/utils.js:180, 201).
- Every channel in the result carries `peer`, and from here on the rebalancer
  works with **peer pubkeys**: queue items, in-flight checks, backoff keys and
  fee lookups are per peer, and bos picks the channel. With two channels to one
  peer, the last one wins in every map.

### 3.2 Sizing (service/rebalancer.js:171–305)

Constants: `minToRebalance` 50,000; `minLocal` 1,000,000; `split` defaults to 1.

`in_flight` for a peer is the sum of two things:
- its pending HTLCs on forwards;
- the full requested amount of every running rebalance from or to it (not
  what's left to move). HTLCs belonging to Jet's own rebalances are excluded,
  so nothing is counted twice.

It never includes refills that are queued but not started. The queue has a
per-peer total (`queue.sats`), but nothing calls it.

| Role | Formula |
|---|---|
| source (inbound) | `has = local − (capacity − min(split·capacity, inflow_1d))` |
| target (outbound) | `needs = min(split·capacity, outflow_1d) − local` (in-flight not subtracted) |
| missed target | if a day of failed HTLCs to the channel totals ≥ capacity: `needs = capacity/2 − in_flight`; at most `floor(maxInstances/4)` targets; current local balance is ignored |
| low-volume target | `needs = min(1M, 20%·capacity) − (local + in_flight)` |
| low-volume source | `has = min(local, capacity/2) − 1M` |

### 3.3 Queue (same loop), stopping at `maxInstances` (10) running + queued

1. **regular**: for target *i* of *n* (sorted by `p`), every source contributes
   `min(((n−i)/n)·has, remaining)`, scheduled `60·i` s out. A source's `has` is
   not reduced between targets, so one source can be promised several times.
2. **missed**: `min(has, remaining)` per source.
3. **forward**: each forward of ≥ 50,000 sats in the last 120 s queues a refill
   of the same amount from the incoming channel's peer to the outgoing
   channel's peer, i.e. it immediately undoes the flow it just earned on. Up to
   `floor(maxInstances/4)`. Any classification qualifies.
4. **low volume**: balanced peers that have sats feed those that need them;
   here the source's remainder *is* tracked.

The queue persists between cycles and is ordered by start time. An item's
amount and max ppm are fixed when it is planned, and it may start up to 9 min
later (`60·i` s). A pair already queued or running is not planned again. But
because sizing ignores the queue, one target can take refills from several
sources in the same cycle, or in later cycles while earlier items still wait,
each sized against the same balance.

### 3.4 Gate per pair (`checkPeers`, rebalancer.js:496)

- one rebalance in flight per (from, to) pair;
- backoff: `n` consecutive failures for the pair in the last 2 h means waiting
  `2·2ⁿ` min from the last attempt (timeouts write no row, so they never back
  off);
- fewer than `maxPendingHtlcs` (4) pending HTLCs on either peer;
- no fee data for the target yet: skip;
- fee analysis of the target (§5): pause, or a max ppm.

### 3.5 Dispatch (every 10 s)

Takes due items off the in-memory queue, enforces the daily ceiling and spawns
rebalance-proc. The ceiling: `rebalanceFeesCommittedSync` (fees paid by
successes in the last 24 h, plus `amount·ppm` for every active rebalance still
within twice its time limit) plus reservations for processes spawned in the
last 3 min, plus the new item's `amount·ppm`, must stay within `maxDailyFee`
(100,000; 0 disables). An item over the ceiling is dropped, not held, and the
next cycle plans it again if it's still needed. The ceiling alert goes to
Telegram at most once a day. Every due item spawns in the same pass, so up to
`maxInstances` Node processes can start together. The queue lives in memory; a
restart drops it.

## 4. One rebalance (api/rebalance.js + bos/rebalance.js)

A run explores routes under a budget and learns which nodes to avoid as it goes.

**Setup.**

- Resolves `from` and `to` by pubkey or by a unique alias fragment. bos tags are
  disabled (`api/tags.js` exports `{}`).
- **A feasibility check comes first** (api/rebalance.js:85). If the target
  peer's fee toward us (`base/1000 + rate`, from the last matching channel)
  already exceeds the budget ppm, the run stops before trying anything. Within
  25% of the budget it warns that the margin may be too thin.
- **The budget** is `max_fee = amount·ppm/1e6`, recomputed from the remaining
  amount at each iteration. The per-hop limit is `min(350, 0.75·ppm)`.
- **The avoid list** is `config.avoid` plus `rebalance_avoid` rows for this pair
  that were learned at a max ppm at or above this run's, within the last
  `max(60, 5·maxTime)` min. Learned exclusions are scoped to the pair and the
  budget, and they expire.
- Inserts an `active_rebalance` row keyed by pid.
- **Mode.** "Aggressive" when the time limit is under 60 min, as the default 30
  is; "patient" otherwise.

**The loop** runs until time is up (`maxTime`, default 30 min), the target is
met, or an error ends it. Each iteration calls bos `manageRebalance` with
`out_through`, `in_through`, `max_fee`, `max_fee_rate` = ppm, `max_rebalance` =
the remaining amount, `timeout_minutes` = the time left, and `avoid`. bos
reports each probed route through a logger callback, and Jet keeps per-node
statistics of the ppm it sees.

- **Fee too high** (bos found a route, over budget):
  - Every hop of that route goes into `liquidity`: it had the liquidity, at
    that price.
  - The route's cumulative ppm updates the run's cheapest failed price (`min`).
  - Jet picks the costliest hop it may avoid (never an endpoint or a node
    already avoided):
    - If that hop is above the per-hop limit, aggressive mode avoids it at
      once and remembers it for the pair (`rebalance_avoid`).
    - Patient mode avoids it only once it has been seen at least four times
      (`MIN_PPMS_TRIES`) with an average above the limit. Until then it tries to
      skip just that hop.
    - If that hop is at or under the limit, the route is expensive in sum with
      no single expensive hop, and Jet tries to skip just that hop.
  - Skipping a hop means a `FEE_RATE>…` avoid string, which never reaches bos
    (rebalance.js:343, 358), since only node ids are passed (rebalance.js:254).
    So the next call sees the same route, finds the hop already skipped, and
    ends the run.
- **No path found:** a last-ditch widening. Jet avoids every node it has seen
  averaging above the per-hop limit and tries again. If there are none, it counts
  a retry.
- **Errors that end the run:**
  - amount too low;
  - peer not found;
  - no outbound peer with enough balance;
  - probe timeout;
  - an amount bos can't parse;
  - unexpected and unidentified errors.
- **Success may be partial.** Each one adds a `rebalance_history` row with
  `ppm = round(1e6·fee/amount)` and records the route's hops in `liquidity`.
  The loop continues until it is within 50,000 sats of the target.
- `REPS` = 2 bounds only retries that made no progress (nothing left to
  exclude). Exclusions and successes don't count against it, so a run is bounded
  by its time limit.

**The end.**

- **A failure row,** one per run, is written only when nothing moved and the last
  error is one of `rebalanceFeeTooHigh`, `failedToFindPath`, `unexpectedError`
  or `unidentifiedError`. `min` holds the cheapest failed route's ppm, the price
  at which a route existed. `jet rebalance-history` shows it to the operator as
  the minimum max ppm needed, but no automated decision reads it. Timeouts and
  the other errors leave no row.
- **A summary is logged:** the nodes seen above and below the per-hop limit,
  with the ppms observed.
- The run deletes its active row and ends with `process.exit()`.

## 5. Fee analysis (api/analyze-fees.js)

Runs for each target before queueing, and behind `jet analyze-fees` and
`jet monitor --status`. Fees are folded to ppm as `base/1000 + rate`, i.e. as if
every payment were 1M sats. `local` is our fee on the target channel (what
routing out through it earns), `remote` is the peer's fee toward us (the price
of the last hop of a refill). `maxAutoPpm` is 500.

1. Pause if `floor(local/maxAutoPpm) > 10`: read as "the operator is
   accumulating".
2. `optimal = local`, or `maxAutoPpm` if `local` is higher and `enforceMaxPpm`
   is on. **With `enforceMaxPpm` off (the default) `maxAutoPpm` is not a cap**:
   a 2,000 ppm channel refills at up to 2,000.
3. Pause if `ceil(remote/maxAutoPpm) ≥ 10`.
4. With `enforceProfitability` off (the default), if `optimal ≤ remote` and
   `maxAutoPpm > optimal`, raise `optimal` to `min(remote + buffer, maxAutoPpm)`
   (`buffer` 250 ppm). This is what lets Jet pay more than the channel earns.
5. Pause if `optimal ≤ remote` still; suggest a local range
   `[remote + 20, remote + buffer]`.
6. Otherwise return `optimal` as the run's max ppm (adjusted for `--profit` when
   given on the CLI).

Fees are keyed by peer (`listFeesSync` → `feeMap[peer]`). Nothing in 1.7 sets
fees automatically.

## 6. HTLC logger (service/htlc-logger.js)

- `subscribeHtlcEvents({})`: each `link_fail_event` whose wire failure is
  `TEMPORARY_CHANNEL_FAILURE` with detail `INSUFFICIENT_BALANCE` becomes a
  `failed_htlc` row (from_chan, to_chan, sats). Our own payments (incoming channel `0`) are
  skipped.
- `subscribeChannelEvents({})`: only ACTIVE/INACTIVE are stored, in
  `channel_events`, keyed by funding txid (byte-reversed) and output index.
  Type 7 events come from an LND enum value Jet's proto lacks; they are noise.
- **What "missed" means.** `cumulativeHtlcs` sums failed sats per outgoing
  channel. It counts failed attempts, not unique payments, so it is an upper
  bound on demand. On one production node, about a quarter of a month's failed
  volume into two parallel hub channels was the same amount retried through the
  other hub within minutes, and HTLCs of 1M sats or more made up another
  quarter. Probes cannot be told apart at all, because no payment hash is
  stored. `failed_htlc` has no index; the rebalancer scans a day of it every
  2 min.

## 7. Worker (service/worker.js)

| Every | Job |
|---|---|
| 60 s | LND liveness ping; OS stats (memory ≥ 85/90/95% alerts) |
| 1 min | `cleanDbRebalances`: kill rebalance processes running past 2× their limit and drop their rows; `txnLoop` |
| 5 min | channel.db size check (4/8/16 GB thresholds); alerts for channels inactive an hour or more (or for an unknown time), at most hourly per channel |
| 60 min | `bosReconnect`: bos reconnect to disconnected peers |
| 24 h after start, then daily | `dbCleanup` (below) |

**txnLoop** copies LND history into `txn`. Forwards: type `forward`,
`from_chan`/`to_chan`, `amt_in`, `fee`. Payments: only those whose last hop is
this node, i.e. rebalances, as type `rebalance` with `from_chan` = first hop,
`to_chan` = the channel that received the liquidity, and `fee_sat`. Amounts and
fees are whole sats; LND's msat fields are not read. `jet stats`
groups by `to_chan`, so it gives a **per-channel P&L**: fees earned routing out
of a channel minus fees paid to refill it. A `digest` of date and channels makes
inserts idempotent. Paging uses a saved timestamp plus an offset; it is fragile
but self-heals through the unique digest.

**dbCleanup.** Each table is trimmed at most once per 7 days:
`rebalance_avoid` to `max(60, 5·maxTime)` min, `liquidity` to 30 days,
`failed_htlc` to 60 days, `txn` to 60 days (by insert time, not transaction
time). `rebalance_history` and `channel_events` are trimmed only when
`db.maxRebalanceHistoryDepth` / `db.maxChannelEventsDepth` are set, so by
default they grow forever. **If any table was trimmed, the worker then runs
`VACUUM`**, which takes an exclusive lock on the whole db; other services'
writes fail with `SQLITE_BUSY` until it finishes. On a production node with a
database of a few hundred MB, the first vacuum after an upgrade held the lock
for over three minutes. Meanwhile Telegram polls failed, and daddy restarted the
rebalancer because its heartbeat couldn't be written.

## 8. Telegram (service/telegram.js)

- `monitorFees` every 10 min: snapshots peers' fees in `nameval` (`fees`),
  reports changes in the **remote** (peer) fees, records them in
  `fee_history` (skipping zeros), and sends fee-analysis alerts for outbound
  and balanced peers.
- The bot answers `/start` and `/help` by storing the sender's chat id as the
  recipient of all alerts, with no allowlist (telegram.js:201, 211).
- `sendTelegramMessageTimed` throttles repeated alerts through `nameval`.

## 9. Database (db/utils.js, `db/jet.db`)

Every process runs `createTables()` when it first loads db/utils, which
re-issues the four `ALTER TABLE rebalance_history ADD COLUMN` statements and the
pid index, all of which fail after the first time. No schema version, no
indexes on `date`, a new connection per call, SQL built by string
concatenation. The `if (doIt()) doIt()` retry only works with `executeDbSync`.

| Table | Written by | Read by | Kept |
|---|---|---|---|
| `rebalance_history` | rebalance runs | backoff, daily cap, `rebalance-history`, `monitor` | forever by default |
| `rebalance_avoid` | rebalance runs | rebalance runs | ≥ 60 min |
| `active_rebalance` | rebalance runs | rebalancer (in flight), cap, worker | life of the run |
| `failed_htlc` | logger | rebalancer (missed), `htlc-analyzer` | 60 days |
| `channel_events` | logger | inactive alerts, `channel-events`, `list-channels` | forever by default |
| `txn` | worker | `stats` | 60 days |
| `liquidity` | rebalance runs | `probes` | 30 days |
| `fee_history` | telegram | fee analysis, `fee-history` | forever |
| `nameval`, `nameval_list` | everyone | heartbeats, offsets, throttles, fee snapshot | n/a |
| `telegram_messages` | services | telegram | n/a |

## 10. Config (`api/config.json`)

Keys read by the code: `macaroonPath`, `tlsCertPath`, `serverAddress`,
`rpcProto`/`lightningProto`/`routerProto`, `channelDbPath`, `debugMode`,
`avoid`, `telegramToken`; `rebalancer.{maxTime, maxPpm, maxAutoPpm,
maxInstances, maxPendingHtlcs, maxDailyFee, enforceMaxPpm,
enforceProfitability, exclude, minCapacity, buffer, disabled}`;
`db.{maxProbeDepth, maxFailedHtlcDepth, maxRebalanceHistoryDepth,
maxChannelEventsDepth}`. Defaults come from `api/constants.js` through `||`, so a
0 is impossible for most keys (`maxDailyFee` is validated separately). Nothing
validates the file.

## 11. Everything Jet does to the node

| Action | Trigger | Spends |
|---|---|---|
| Circular rebalance (bos pay to self) | rebalancer, `jet rebalance` | fees, under `maxDailyFee` when automated; no cap when manual |
| Reconnect peers (bos reconnect) | worker hourly, `jet reconnect` | nothing |
| Pay an invoice (bos) | `jet pay` | amount + up to `--max-fee` (1,337) |
| Keysend message, 1 sat | `jet send-message` | ≤ 3 sats |
| Set channel policy | `jet update-channel` | nothing |
| Close channel, cooperative or force | `jet close-channel` | on-chain fee |
| Delete payments from LND | `tools/prune-payments` | nothing, but irreversible |
| Kill rebalance processes | worker | nothing |

Everything else (`status`, `peers`, `fees`, `analyze-fees`, `stats`,
`probes`, `monitor`, `info`, `htlc-history`, `htlc-analyzer`,
`rebalance-history`, `fee-history`, `channel-events`, `list-channels`,
`list-peers`, `channeldb`) only reads. `jet rebalance` treats an amount below
1,000 as millions of sats.

## 12. Latent defects found in the read (not fixed in 1.7.0)

- **Routing history.** `htlcHistorySync` caps at 10,000 forwards with no
  paging, blocks forever on a gRPC error, and reads `peers[…].name` unguarded
  (lnd-api/utils.js:259, 287): one channel whose peer has no node announcement
  throws and aborts the whole rebalancer iteration. `htlcAnalyzer` has the same
  unguarded lookups (api/htlc-analyzer.js:268, 272, 293).
- **bos debug callbacks** reference an undefined variable:
  `args.logger.debug(debug)` (bos/rebalance.js:59, bos/pay.js:53). It throws
  whenever bos logs at debug level.
- **Per-hop fee skips never reach bos** (api/rebalance.js:254). When the
  costliest hop is within the per-hop limit, the run repeats the same route once
  and ends, instead of exploring further (§4).
- **A run's closing summary can crash it** (api/rebalance.js:543–544). It reads
  node info without a guard, outside the error handling, so a node missing from
  the graph aborts the run before it deletes its active row. The worker removes
  the row within a minute.
- **The feasibility check leaves no trace** (api/rebalance.js:88–89). A run that
  stops there writes neither an active row nor history, so nothing backs off
  from it. Fee analysis normally keeps the rebalancer from planning such a run,
  so in practice this affects manual `jet rebalance`.
- **Dead code:** `routes` (api/rebalance.js:102) is never filled, so the
  summary's routes list is always empty.
- **Sizing ignores the queue** (service/rebalancer.js:650, and
  service/queue.js:34, which nothing calls). One target can be promised refills
  from several sources at once, each sized against the same balance (§3.3).
- **The "no peers" guard on the exclude list can't fire**
  (service/rebalancer.js:80). It tests `peerMap.length` on an object. With no
  peers, every exclude entry is reported unknown instead.
- **`jet update-channel` ignores `failed_updates`** in LND's reply and prints
  "done" for a policy change that failed (lnd-api/update-channel.js:52).
- **`tools/prune-payments`** proceeds on any answer except `n`, including a
  bare Enter (tools/prune-payments:32).
- **`api/pay.js` sets `global.testModeOn = true`** at load (line 3). Only the
  `pay` command loads it, so the effect is extra console output there.
- **Stuck-rebalance cleanup trusts a bare pid** (service/worker.js:178–214,
  api/utils.js:280). A leftover `active_rebalance` row (after a reboot, an OOM
  kill or a power loss) whose pid now belongs to some other process is treated
  as a stuck rebalance: the worker sends a Telegram alert and SIGTERMs that
  process. If the process belongs to another user, `kill` throws before the row
  is deleted, so the alert repeats every minute.
- **Telegram `/start`** hands the alert stream to whoever sends it.
- **Unused code:** `classifyPeersAnalyzer` (api/htlc-analyzer.js:18), the whole
  of `api/tags.js` (commented out, exports `{}`).
- **`channeldb.js`** runs `find` over the LND directory at require time to
  locate channel.db when `channelDbPath` is unset.
- **Worker freeze after the hourly reconnect.** Observed in production: the
  worker logged "reconnecting peers" and then nothing, not even its one-minute
  txn sync, until daddy restarted it 5 minutes later for a stale heartbeat. The
  cause is unknown; the reconnect is async, so something blocked the event loop.
- Already in 1.7's `BACKLOG.md`: `recordLiquidity` collisions (21), and
  timed-out runs leave no history (24).

## 13. What this means for 2.0

The detail behind each point is above. The design response is in the
[architecture overview](../architecture/overview.md).

- **Keep the signals.** These are:
  - real routing volume per channel;
  - failed HTLCs on our balance, stored with the payment hash so that retries
    and probes can be told apart;
  - the measured cost of every refill (`txn`, with channel ids), at msat
    precision in 2.0;
  - hop prices discovered while probing (`liquidity`).
- **Change the unit.** Plan per channel and spend against each channel's own
  revenue. 1.7 classifies per channel, acts per peer, and lets the refill
  budget exceed the channel's own fee.
- **Close the loop.** Read back the cheapest failed route, and back off on
  timeouts too. Reservations should be durable, not in-memory.
- **One process, real async, deadlines on every RPC, an explicit schema with
  indexes and versions, validated config.** No `VACUUM` behind the operator's
  back.
- **The action surface is small:** pay-to-self, reconnect and policy updates
  are all it needs, which is what a Foundry Policy and a scoped macaroon have to
  cover.
