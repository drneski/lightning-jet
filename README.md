# Lightning Jet 2.0

> **This is the `v2` branch: a ground-up rebuild, in development.** It reads a
> node, records its HTLC and channel events, and records what it would do, in
> shadow mode; it changes nothing on the node. Lightning Jet 1.7 is on
> [`main`](https://github.com/drneski/lightning-jet/tree/main), and it is the
> version to run today.

Lightning Jet is an autonomous economic engine for LND routing nodes. 2.0 starts
with fee management and circular rebalancing. Both are decided from one
per-channel model of what liquidity earns and what it costs, inside deterministic
limits, and every decision is recorded and explainable. When the evidence doesn't
support acting, Jet says so and does nothing.

## Status

The architecture is settled; see
[`docs/architecture/overview.md`](docs/architecture/overview.md). Built so far:

- **The read-only vertical slice.** Each cycle observes every channel,
  estimates its economic position with explicit uncertainty, proposes a fee
  experiment or abstains, authorizes it in shadow mode, records the decision,
  and can explain it.
- **The observe phase.** Jet records LND's HTLC and channel events as they
  happen, including the forwards that failed for lack of balance, with any
  gaps in the record known. It reads competitors' advertised fees from the
  graph as a prior. It runs unattended under a hardened systemd unit.

The records and rules it follows are in
[`docs/architecture/decision-contracts.md`](docs/architecture/decision-contracts.md).

## Trying it

It is read-only and shadow-only: it reads LND with the read-only macaroon,
records what it would do, and changes nothing. Put the LND settings in a config
file; [`contrib/jet.conf.example`](contrib/jet.conf.example) lists every key.
Jet looks for it at `$XDG_CONFIG_HOME/lightning-jet/jet.conf`, or wherever
`--config` says, and flags override it. The database lives in
`$XDG_DATA_HOME/lightning-jet` unless `data_dir` or `--data-dir` says
otherwise.

Run one cycle and exit:

```bash
jet run --once
```

Run continuously, recording LND's event streams, with a cycle every hour:

```bash
jet run
```

Then:
- `jet status` summarizes the latest cycle and what is being recorded;
- `jet explain <channel id>` explains a channel's latest decision;
- `jet health` checks that cycles and both streams are current, and exits 1 if
  they aren't.

## Running under systemd

[`contrib/systemd/jet.service`](contrib/systemd/jet.service) runs Jet as an
unprivileged dynamic user, with a read-only file system apart from
`/var/lib/lightning-jet`. Home directories are hidden, and the drop-in binds
back in only LND's `tls.cert` and `readonly.macaroon`. Fill in the drop-in
from
[`contrib/systemd/lnd-paths.conf.example`](contrib/systemd/lnd-paths.conf.example)
with your node's paths, and put the same paths in `/etc/lightning-jet/jet.conf`.
Then:

```bash
sudo install -D -m 0755 jet /opt/lightning-jet/jet
sudo install -D -m 0644 contrib/systemd/jet.service /etc/systemd/system/jet.service
sudo install -D -m 0644 lnd-paths.conf /etc/systemd/system/jet.service.d/lnd-paths.conf
sudo install -D -m 0644 jet.conf /etc/lightning-jet/jet.conf
sudo systemctl daemon-reload
sudo systemctl enable --now jet
```

Check it with:

```bash
sudo /opt/lightning-jet/jet health --config /etc/lightning-jet/jet.conf --data-dir /var/lib/lightning-jet
```

The unit restricts the network to localhost where the kernel supports cgroup
BPF. Where it doesn't, systemd says so and runs without that restriction.

## Building

Requires Go 1.27.2 or later.

```bash
go build ./cmd/jet
go test ./...
```

## How it is built

Jet 2.0 is AI-native in how it is built as well as in what it does: every commit
is written with AI assistance and names the model in a `Co-Authored-By` trailer.
[`AGENTS.md`](AGENTS.md) has the rules for anyone contributing, human or AI.

## Lightning Foundry

[Lightning Foundry](https://github.com/OogaBoogaX/lightningfoundry) runs and
supervises a node's stack. Jet is the independent engine that decides and acts.
In Foundry's managed mode, every action Jet takes is authorized by Foundry's
policy before LND carries it out.

## License

[MIT](LICENSE)
