# Lightning Jet 2.0

> **This is the `v2` branch: a ground-up rebuild, in development.** It reads a
> node and records what it would do, in shadow mode; it changes nothing on the
> node. Lightning Jet 1.7 is on
> [`main`](https://github.com/drneski/lightning-jet/tree/main), and it is the
> version to run today.

Lightning Jet is an autonomous economic engine for LND routing nodes. 2.0 starts
with fee management and circular rebalancing. Both are decided from one
per-channel model of what liquidity earns and what it costs, inside deterministic
limits, and every decision is recorded and explainable. When the evidence doesn't
support acting, Jet says so and does nothing.

## Status

The architecture is settled; see
[`docs/architecture/overview.md`](docs/architecture/overview.md). The first
milestone is a read-only vertical slice:

1. observe a channel;
2. estimate its economic position, with explicit uncertainty;
3. propose an action or abstain;
4. authorize it in shadow mode;
5. record the decision and explain it.

The records and rules it follows are in
[`docs/architecture/decision-contracts.md`](docs/architecture/decision-contracts.md).

## Trying the slice

It is read-only and shadow-only: it reads LND with the read-only macaroon,
records what it would do, and changes nothing. Its database lives in
`$XDG_DATA_HOME/lightning-jet` (by default `~/.local/share/lightning-jet` on
Linux and `~/Library/Application Support/lightning-jet` on macOS), or wherever
`--data-dir` says.

```bash
jet run --once --lnd localhost:8080 --tls-cert /path/to/tls.cert --macaroon /path/to/readonly.macaroon
```

```bash
jet status
```

```bash
jet explain <channel id>
```

Without `--once`, `jet run` repeats every hour (`--interval`) until stopped.

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
