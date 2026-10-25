# Security policy

Lightning Jet acts on nodes that hold real money. We would much rather hear about
a problem from you than from an operator who lost funds.

## Jet 2.0 is in development

Nothing on this branch is released, and nothing here should be run against a
funded node. Lightning Jet 1.7, on
[`main`](https://github.com/drneski/lightning-jet/tree/main), is the version in
use today, and reports about it are welcome too.

A routing node can lose money while every component behaves exactly as designed:
through fees, capital tied up in channels, and force closes. That is not a
vulnerability.

## Reporting a vulnerability

**Report privately. Do not open a public issue for a security problem.**

Use GitHub's private vulnerability reporting:
[report a vulnerability](https://github.com/drneski/lightning-jet/security/advisories/new),
or go to Security → Report a vulnerability on the repository page. It opens a
private thread that only the maintainers can see.

It helps to include, as far as you have them:

- what an attacker gains, not only what misbehaves;
- the smallest reproduction you can manage, and the versions involved;
- whether the problem is already public or being exploited;
- how you would like to be credited, or that you'd rather not be.

Please don't include seeds, macaroons, private keys, node pubkeys or channel
points in a report. If a reproduction seems to need them, say so and we'll find
another way.

## What we will do

This is a small project without a security team, so these are realistic aims
rather than promises:

- acknowledge a report within three days;
- give an initial assessment within two weeks;
- tell you plainly if a fix will take longer, or if we decide not to fix
  something, and why.

We ask for a reasonable window to ship a fix before public disclosure, and we'll
agree the timing with you. If a problem is being exploited, disclosure speed
matters more than our schedule.

## Scope

**In scope:**

- handling of macaroons and other credentials;
- the limits that protect capital, and any way around them, including by a
  strategy or AI component;
- the boundary between proposing an action and authorizing it;
- the release and verification path;
- leaks of a node's operational data.

**Out of scope:**

- vulnerabilities in LND, Bitcoin Core or Lightning Foundry themselves (please
  report those to their maintainers, though Jet *using* them unsafely is in
  scope);
- the fact that routing nodes can lose money;
- attacks that need physical access to the operator's hardware.
