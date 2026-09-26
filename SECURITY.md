# Security policy

## Reporting a vulnerability

Please report it privately through GitHub: **Security → Report a
vulnerability** on this repository. That opens a private advisory that only
the maintainers can see, where we can talk it through and credit you when a
fix ships.

Please do not open a public issue or pull request for it. If private reporting
is not available to you for some reason, open an issue that says only that you
have something to report, and we will give you a private channel.

Useful to include: the version or commit, what an attacker needs (network
access, a key, an account), what they get, and steps to reproduce.

## What to expect

Tracepad is maintained by a small team, so this is best effort. We read every
report and reply once we have looked at it, usually within a week. We fix
confirmed issues in the next release and say in its notes what was fixed.
We do not run a bug bounty.

## Supported versions

Tracepad is before 1.0. Fixes go into the newest release of each artifact —
the server (`vX.Y.Z` tags, archives and the `ghcr.io/tracepad/tracepad` image)
and the Python, Node and Go packages. Older releases do not get fixes; upgrade
to the newest to pick one up.

## Scope

The server, its web interface, CLI and MCP server, the three SDK packages and
the release artifacts built from this repository. Keys, accounts and the admin
token are described in [docs/accounts.md](docs/accounts.md) and
[docs/admin.md](docs/admin.md).
