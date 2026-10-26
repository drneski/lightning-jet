# Decision contracts

What stays stable across Jet 2.0's economics engine, fee controller, rebalancer and
future AI strategies: the records every decision produces, and the rules they
obey. Subsystems change behind these contracts; the contracts change only by
versioned, additive revision.

This document does not design the subsystems. Payment execution (attempts,
payment hashes, recovery) gets its own specification before any engine writes to
a node; see [Out of scope](#out-of-scope).

## Invariants

1. **No action without an authorized intent.** Jet changes nothing on a node
   unless a recorded intent has been approved by deterministic authorization.
   Retries, partial attempts and recovery after a crash all belong to the same
   intent, inside its original limits, budget reservation and expiry. Anything
   beyond them needs a new intent.
2. **Every decision is reconstructable.** For every intent and every abstention,
   Jet can show the inputs it saw, the strategy and version that reasoned over
   them, the authorization rules applied and their results, and what happened
   afterwards. Records are append-only; a correction is a new record that
   references the old one.
3. **Insufficient evidence is a valid outcome.** Doing nothing because no action
   is profitable, doing nothing because the evidence can't support a forecast,
   and doing nothing because the inputs can't be trusted are three different
   decisions. They are recorded and explained as such. On a quiet node,
   inactivity is often the right answer, and the records must show that it was
   reasoned rather than idle.

Strategies propose and authorization decides. A strategy, including a learned
model, can only produce intents and abstentions. It never sets or relaxes a
limit (Lightning Foundry's
[invariant 6](https://github.com/OogaBoogaX/lightningfoundry/blob/rock/docs/invariants.md#6-deterministic-security)).

## The cycle

```
snapshot ──► estimates ──► strategy ──┬─► intent ──► authorization ──► decision record ──► outcomes
                                      └─► abstention
```

Each evaluation of a channel by a strategy ends in exactly one intent or exactly
one abstention. Authorization runs the same way in every mode. In `shadow` mode
an approved intent is recorded and never executed. In `live` mode it goes to
execution.

## Conventions

- **Money** is in millisatoshis, as signed 64-bit integers, in fields ending in
  `_msat`. Fee rates are integer parts per million (`_ppm`). No floating point
  touches an amount.
- **Channels** are identified by their short channel id, a 64-bit integer written
  as a decimal string in JSON (`"chan_id": "…"`), because it exceeds the range
  some JSON readers handle exactly. Peers are 33-byte public keys in hex.
- **Time** is UTC. JSON uses RFC 3339 at millisecond precision, with a zero
  fraction left out (`2026-10-26T00:00:00Z`). Durations are whole
  hours (`_hours`).
- **Identifiers** are UUIDv7, which are unique and time-ordered. They are
  internal: anything Jet ever publishes gets fresh random ids, as in Foundry's
  [ADR 0003](https://github.com/OogaBoogaX/lightningfoundry/blob/rock/docs/decisions/0003-random-public-ids.md).
- **Versions.** Every record names its schema (`"schema": "jet.intent/1"`). A
  revision may add optional fields; anything else is a new major. Strategies
  and estimators carry their own version, and authorization records the version
  of the limits it applied.
- **Observed and derived stay apart.** A value read from LND and a value Jet
  inferred never share a field, and every derived value says how it was derived.
- **Privacy.** Records describe the node's channels, balances and earnings. They
  stay in the node's local database and are never committed, exported or
  published. Tests and examples use synthetic values only.

## Two separate scores: data quality and economic confidence

Every estimate carries both, and nothing combines them into one number.

- **Data quality** is about the inputs as observations. Are they present, fresh
  for how often they change, and consistent with each other? A channel with a
  current balance, current fee policies on both sides and a complete forwarding
  history has good data.
- **Economic confidence** is about the forecast. Given those inputs, is there
  enough relevant history to predict what an action would earn or cost? The same
  channel, with three forwards in a month, has low confidence however good its
  data is.

| Data quality | Meaning |
|---|---|
| `good` | Complete, fresh and consistent |
| `degraded` | Usable, with named issues |
| `insufficient` | Must not be used |

| Economic confidence | Meaning |
|---|---|
| `high` | Enough history to forecast within a narrow range |
| `medium` | A forecast with a moderate range |
| `low` | A forecast with a wide range |
| `none` | No forecast is defensible |

Each level comes with its reasons: the issues found, or the sample sizes and
windows behind the forecast. A strategy declares the minimum of each that it
needs to act. Below the data minimum it abstains with `insufficient_data`; below
the confidence minimum, with `insufficient_evidence`.

## Snapshot

The inputs one evaluation saw, frozen so the decision can be reconstructed.

| Field | Meaning |
|---|---|
| `id`, `schema` | `jet.snapshot/1` |
| `node_pubkey`, `block_height` | The node observed, and its chain tip |
| `taken_at` | When the snapshot was assembled |
| `sources` | Each input (channels, policies, forwards, and later failures, refills and gossip) with the time it was read from LND or the local store |
| `channels` | Each channel's balances, both sides' policies, a summary of its forwards over the window, and its data quality: level and issues |
| `content_sha256` | SHA-256 of the snapshot's JSON encoding with this field empty, so a stored snapshot can be shown to be the one a decision used |

Snapshots are stored locally under a retention policy. A record that outlives
its snapshot keeps the hash and every value `jet explain` shows, so the
explanation survives the snapshot.

## Estimate

What an estimator believes about one channel, with how much it should be
believed.

| Field | Meaning |
|---|---|
| `id`, `schema` | `jet.estimate/1` |
| `chan_id` | The channel |
| `kind` | What is estimated, such as `outbound_liquidity_value` or `replacement_cost` |
| `snapshot_id` | The inputs used |
| `estimator` | Name and version |
| `horizon_hours` | The period the estimate covers |
| `expected_msat` | What the kind defines. A value estimate gives the expected **incremental** value over the horizon, compared with doing nothing. A baseline, such as `outbound_revenue` (what the channel earns if nothing changes), gives the expected amount. A cost estimate gives the expected cost. |
| `low_msat`, `high_msat`, `interval` | The range, and what it is (for example `p10-p90`). At economic confidence `none`, all three value fields are omitted: there is no forecast to state. |
| `economic_confidence` | Level and basis (sample sizes, windows) |
| `data_quality` | Level and issues, carried from the snapshot for the inputs used |
| `evidence` | The observations behind it, each with its kind, window, count and source, including priors such as gossip fees, labeled as priors |
| `assumptions` | Explicit statements the estimate depends on, such as "the peer keeps its current fee" |
| `computed_at` | When |

"Incremental" is the point. An estimate that can't say what would happen without
the action has to say so through its confidence, not by quietly crediting the
action with revenue the channel would have earned anyway. Baselines are what
incremental value is measured against.

## Intent

A proposed action, specific enough to authorize, execute and audit.

| Field | Meaning |
|---|---|
| `id`, `schema` | `jet.intent/1` |
| `action` | `fee_update` or `rebalance` (more later) |
| `target` | The channel acted on; for `rebalance`, also the source channel |
| `parameters` | Per action. `fee_update` has `fee`: `base_msat` and `fee_ppm`, with the `observed_base_msat` and `observed_fee_ppm` they replace. `rebalance` will have the amount to move. |
| `limits` | The bounds execution must stay inside. `rebalance`: maximum fee (`max_fee_msat`) and time limit. `fee_update`: none beyond the parameters. |
| `rationale` | The estimate ids it rests on, and a one-line summary generated from them |
| `strategy` | Name and version |
| `snapshot_id` | The inputs used |
| `mode` | `shadow` or `live` at the time of proposal |
| `created_at`, `expires_at` | No attempt starts after `expires_at`; one already in flight runs to its final state |

An intent is immutable. Execution attempts reference it, and the payment
execution specification will define them.

## Abstention

A recorded decision to do nothing, with the reason.

| Field | Meaning |
|---|---|
| `id`, `schema` | `jet.abstention/1` |
| `chan_id` | The channel |
| `strategy` | Name and version |
| `snapshot_id`, `estimate_ids` | What it saw |
| `reason` | `no_profitable_action`, `insufficient_evidence` or `insufficient_data` |
| `detail` | The specific shortfall: which estimate fell short of which threshold, or which input was missing or stale |
| `would_change_if` | The conditions under which the strategy would act, stated concretely, such as "at least 10 outbound forwards in 30 days" |
| `revisit_at` | The next scheduled evaluation |
| `created_at` | When |

## Decision record

Authorization's verdict on one intent.

| Field | Meaning |
|---|---|
| `id`, `schema` | `jet.decision/1` |
| `intent_id` | The intent judged |
| `snapshot_id` | The inputs authorization used: the intent's, unless authorization ran later and re-read state |
| `verdict` | `approved` or `rejected` |
| `reason` | For a rejection, the rule or rules that failed, with their numbers |
| `rules` | Every rule evaluated, as `rule`, `passed` and `detail`, not just the failing ones |
| `policy_version` | Hash of the effective limits configuration |
| `authorizer` | `standalone` now; `foundry` once managed mode exists |
| `mode` | `shadow` or `live` |
| `reservation_msat` | The budget reserved for an approved intent. In live mode it is held until the intent's final state. In shadow mode the full reservation is booked as hypothetical spend in a separate shadow ledger, so later shadow decisions meet the limits a live run would have met, without touching live budgets. |
| `decided_at` | When |

### Outcomes

An outcome (`jet.outcome/1`: `id`, `intent_id`, `state`, `detail`, `at`) is
appended to the intent each time its state changes, so the decision record
itself stays immutable:

| State | Meaning |
|---|---|
| `not_executed` | Shadow mode, or rejected |
| `pending` | Approved in live mode; execution started |
| `succeeded` | Completed; carries the actual cost and liquidity moved, or the policy as read back from LND |
| `failed` | Definitively failed; carries the reason |
| `expired` | Reached `expires_at` before any attempt started |
| `canceled` | Withdrawn, with the reason |

In live mode the reservation is released only on a final state, and a timeout
is never a final state. The current outcome is the latest one.

Whether an action paid off over its horizon is a later **evaluation** record,
which compares realized value against the estimate and against a baseline.
Every intent and abstention is shaped so that it can be evaluated; the
evaluation contract follows once there are outcomes to evaluate.

## `jet explain`

What Jet tells the operator about a channel or an intent. It reads recorded data
only and never recomputes, so the explanation is of what actually happened, not
of what the current code would do now.

It answers five questions in order:

1. **Observed.** What Jet saw: the snapshot time, the key facts (balance, our fee,
   the peer's fee, recent forwards and failures), and data quality, input by
   input.
2. **Estimated.** Each estimate's expected value, range and horizon, with
   economic confidence and data quality shown separately, and its assumptions.
3. **Proposed.** The intent (action, parameters, limits, expiry), or the
   abstention and its reason.
4. **Decided.** The verdict, every rule evaluated, and the mode.
5. **Next.** The outcome so far, when the channel is evaluated again, and what
   would change the decision.

`jet explain --json` emits the same content as structured data, which is the
stable contract. The text form is presentation and may change. An optional
language model may later narrate the JSON, in a separate process. It never
alters the JSON or adds facts that aren't in it.

## Example

A synthetic quiet channel. Its data is good, but it has too little history to
forecast whether a lower fee would earn more, so the strategy abstains.

```json
{
  "schema": "jet.abstention/1",
  "id": "01a17581-c000-7d1a-99b6-e2f0c1d3e4a5",
  "chan_id": "111111111111111111",
  "strategy": {"name": "fee.balance-band", "version": "0.1.0"},
  "snapshot_id": "01a17581-b830-7c3b-98d2-e6a4b5c6d7e8",
  "estimate_ids": ["01a17581-bc18-7e4c-99f1-a3b2c4d5e6f7"],
  "reason": "insufficient_evidence",
  "detail": "outbound_liquidity_value has economic confidence low (3 outbound forwards in 30 days); this strategy needs medium",
  "would_change_if": [
    "at least 10 outbound forwards through this channel within 30 days",
    "a fee experiment is authorized for this channel"
  ],
  "revisit_at": "2026-11-02T00:00:00.000Z",
  "created_at": "2026-10-26T00:00:00.000Z"
}
```

```
$ jet explain 111111111111111111
Observed    2026-10-26 00:00 UTC. Local 4,500,000 sats of 5,000,000 (90%).
            Our fee: base 0, 500 ppm. Peer's fee to us: base 0, 400 ppm.
            30 days: 3 forwards out, 0 failed for balance.
            Data quality: good (balances 2 min old, policies current, history complete).
Estimated   Outbound liquidity value, 14 days: +40 sats expected, -150 to +600 (p10-p90).
            Economic confidence: low (3 forwards in 30 days); this strategy needs medium.
            Gossip prior: similar routes advertise 200-350 ppm (a prior, not a price).
Proposed    Nothing. Insufficient evidence to forecast whether a lower fee earns more.
Decided     No intent, so no authorization.
Next        Evaluated again 2026-11-02. Would act with at least 10 outbound forwards
            in 30 days, or if a fee experiment is authorized for this channel.
```

## Out of scope

- **Payment execution:** attempts, the self-invoice and payment hash per
  attempt, reconciliation with LND after a crash or timeout, single-part
  payments, and per-HTLC arrival accounting. These get their own specification
  before the rebalancing engine writes anything.
- **The Foundry interface:** how intents and verdicts map onto Foundry's
  schema in managed mode. The fields above are chosen so that mapping is
  direct.
- **Evaluation records,** described above, once there are outcomes to
  evaluate.
