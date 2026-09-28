# Spec 046 — Rate limiting by source: who is asking the server to check a password

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Spec 028 #31 bounded what an unauthenticated caller can make the server
> spend on passwords: at most four `bcrypt` comparisons at once, a short queue
> behind them, `503` past it. It also stated the price. A flood of about
> sixteen sign-ins a second, each with a different email, trips no email's
> limit and keeps the gate full. While it runs, every real sign-in, setup,
> invitation and password change answers `503`. Nothing in the server can tell
> the flood from people, because nothing knows where a request comes from. The
> address it reads today is either the TCP peer, which behind a proxy is the
> proxy, or the first hop of `X-Forwarded-For`, which the client writes. This
> spec gives the server one trustworthy notion of a request's source: the TCP
> peer, unless that peer is a proxy the operator named, and then the hop that
> proxy vouches for. It limits, per source, the one kind of work the password
> gate exists to protect. A flood from one address, or one IPv6 /64, then
> costs the gate one comparison every three seconds, and the people signing in
> from anywhere else never meet it.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- **The source of a request** (Decisions 1–5): `TRACEPAD_TRUSTED_PROXIES`, a
  list of addresses and CIDR ranges, loopback by default. `X-Forwarded-For` is
  read right to left through the trusted hops. IPv6 is aggregated to its /64.
  One resolver serves the limit, the session list and the log lines that name
  a client.
- **A per-source limit on password work** (Decisions 6–11): every request that
  would take a place at the password gate first spends a token from its
  source's bucket. GCRA holds 20 in a burst and refills one every 3 seconds.
  Past that, `429` with `Retry-After`, before the gate, and the email
  limiter's reservation is given back. At most 32,768 sources are held, evicted by
  least debt. Refusals are logged through the shared log pacer (`logpace.Keyed`).
- **What an operator sees** (Decisions 12–13): a `WARN` when a request
  carrying `X-Forwarded-For` arrives from an untrusted private or loopback
  peer, which is a proxy nobody named. `GET /api/v1/system` reports the
  source the server resolved for the request that asked.

Not limited here, on purpose (Decisions 14–15): wrong `Bearer` credentials,
and ingest.

Builds on spec 028 #26 (the public routes' body limits), #29–#30 (origin
checks: which *host* the server is, which this spec does not touch) and #31
(the password gate and the email limiter), spec 001 #12 and #15 (the
listener and the transport) and spec 043 (the body budget, read slots and
deadlines, and Decision 22's split between settings and constants).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-28** — *Rationale amended by #16.* **A request's source is its TCP peer, unless the peer is a trusted proxy.** `TRACEPAD_TRUSTED_PROXIES` is a comma-separated list of IP addresses and CIDR ranges, IPv4 and IPv6. Its default is **`loopback`** (`127.0.0.0/8`, `::1/128`). `none` trusts nothing, loopback included. Whitespace around entries is ignored. An entry that does not parse refuses to start, naming it and its position. The start logs the list in effect once, at `INFO` | The peer is the one address a client cannot choose. A header is only as good as whoever wrote it, and only the operator knows which peers are their proxies. Loopback by default because a proxy on the same host (a Caddy or nginx beside a binary) is the deployment the docs recommend outside Docker. Anybody who can connect over loopback already runs code on the machine, so trusting what they forward gives nothing away. `none` exists for an operator who wants the peer and only the peer. |
| 2 | **2026-09-28** — **`X-Forwarded-For` is read right to left.** All of the request's `X-Forwarded-For` lines are joined in order, as one comma-separated list. Starting at the peer, while the current address is trusted, step to the next entry from the right. The first address that is not trusted is the source. An entry that is not an IP address (empty, `unknown`, an obfuscated identifier, garbage) stops the walk, and the last trusted address becomes the source. At most **32** entries are read from the right; past them, the last trusted address is the source. An entry may carry a port (`203.0.113.7:51234`, `[2001:db8::1]:443`), which is dropped. So is an IPv6 zone. An IPv4-mapped IPv6 address is read as the IPv4 address | Each proxy appends the peer it saw, so an entry is only as trustworthy as the hop to its right. The rightmost untrusted entry was written by a proxy we trust, about a peer we do not; everything to its left was written by that peer and may be invented. Reading the *first* hop, as `clientIP` does today, lets any client pick its own source, which for a limit means a fresh bucket per request. Stopping at garbage, rather than skipping it, keeps an invented entry from moving the walk further left. The 32-entry bound keeps a header of thousands of entries from being a parser workload: no real chain of proxies is that deep. Ports and zones name the same client; mapped addresses are how a dual-stack socket spells an IPv4 peer. |
| 3 | **2026-09-28** — **`Forwarded` (RFC 7239) and `X-Real-IP` are not read.** Only `X-Forwarded-For` | A proxy that appends to one header passes the others through as the client sent them. If the server read two headers, a client could write the one the proxy leaves alone, and the source would be whatever it chose. nginx's documented configuration (`$proxy_add_x_forwarded_for`), Caddy, Traefik and the cloud load balancers all write `X-Forwarded-For`. One header, named in the docs, is one thing to get right. |
| 4 | **2026-09-28** — **An IPv6 source is its /64; an IPv4 source is the address.** Constants, not settings | One IPv6 host is routinely given a whole /64, and SLAAC privacy addresses rotate inside it, so per-address buckets would give one machine 2⁶⁴ of them. A /64 is the smallest block one subscriber is expected to hold. IPv4 has no such block: a /24 is often a whole office or a carrier-grade NAT. What is a *source* shapes the answer and belongs to no deployment's tuning (spec 043 #22). |
| 5 | **2026-09-28** — **One resolver for every use of a client's address.** The session list's `ip` (spec 028), the limit, and every log line that names a client read the source through the same function. `clientIP`'s first-hop reading goes | Two readings of one fact drift. The session list is where a person decides whether they recognise a sign-in; showing them an address the client chose was harmless only while nothing else depended on the same code. Behind a proxy that is not configured, the list now shows the proxy's address. That is true, and Decision 12's warning says what to set. |
| 6 | **2026-09-28** — **What is limited is password work, at the gate.** A request spends a token from its source's bucket where it would enter the password gate (`enterPasswordGate`, spec 028 #31 b), before it takes a place or a spot in the queue. That covers sign-in (the decoy comparison of an unknown email included), setup, accepting an invitation and a password change: every request that runs `bcrypt`, and nothing else. A request refused earlier spends no token. That includes an over-long email (#26), an email the email limiter has locked out, an invitation token that does not exist (#31 d) and a body the router refused | The failure spec 028 #31 recorded is the gate being full, so the gate is where the source should be asked. Tied to the gate, the limit is exact: a source can spend at most its rate in comparisons, whichever route it arrives by, and a route added later that hashes a password is limited without anybody listing it. The gate's entry is already the one function every `bcrypt` caller passes through, enforced by the `PasswordSlot` type. The cheap refusals in front of it cost less than a bucket lookup would, so charging them buys nothing. |
| 7 | **2026-09-28** — **GCRA, 20 at once, one every 3 seconds.** Each source holds one instant, its theoretical arrival time (TAT). With `T` = 3 s and `τ` = (20 − 1) × `T` = 57 s, a request at `now` is admitted when `max(TAT, now) − now ≤ τ`, and TAT becomes `max(TAT, now) + T`. A refused request leaves TAT where it was. Time is the monotonic clock. The three numbers are constants | GCRA is a token bucket kept as one timestamp: exact, constant time, no timer, and nothing to refill. Twenty covers a team arriving at once from one office's NAT, and the setup-then-invite-then-sign-in of a first morning. One every three seconds (twenty a minute) is more than any person types, and a quarter of one core's worth of `bcrypt` in the worst case. Refusals do not push TAT on, so a source that keeps hammering gets exactly the sustained rate, not a lockout it extends itself. Constants because they shape what a client meets, as the email limiter's five in fifteen minutes does (spec 028 #8); a deployment that needs other numbers has a proxy in front of it that can say so. |
| 8 | **2026-09-28** — **Refused: `429`, `Retry-After`, and the email limiter's reservation given back.** The body is `{"error": "too many password checks from your network; try again in N seconds"}`. `Retry-After` is `N`: the seconds until one token is back, rounded up, at least 1. A sign-in that had reserved its attempt against the email (#31 a) gives it back as any attempt turned away before the comparison does. A password change's per-account reservation is given back the same way. The `503` of a full gate is unchanged, and spends the token that got it there | A `429` says what happened and when to come back; the `503` stays the answer for "the server is busy with other people". The reservation is given back so that a person stopped by their network's limit does not also lose one of their email's five. The token spent on a full gate is not refunded: it bought a place in line, a refund would need TAT arithmetic that races with concurrent requests, and twenty covers the retries. |
| 9 | **2026-09-28** — **At most 32,768 sources; the least debt goes first.** Sources are kept in a map and a min-heap ordered by TAT. A source whose TAT is at or before `now` holds a full bucket, which is the same as not being held, so every operation first pops those from the heap. They leave without changing any answer. Past the ceiling, the source with the smallest TAT is evicted: the one that would be whole again soonest. About 100 bytes a source, about 3 MB at the ceiling | The lesson of spec 028 #31 c was a limiter that evicted by a count that had gone stale, so a map full of old records outranked a live one. A TAT cannot go stale: ordering by it never changes as time passes, and an expired entry is exactly an absent one. Evicting least debt means that to free a source with debt `d`, an attacker must hold 32,768 other sources each owing more than `d`, inside the 60 seconds any debt lasts. That is 32,768 admitted password checks a minute, against a gate that passes about 960. |
| 10 | **2026-09-28** — **Refusals are logged through the shared log pacer (`logpace.Keyed`, spec 043's `logLimiter` as merged in #126), keyed by source**: at most one `WARN` a minute per source, for at most 16 sources a minute, each with the count it stands for. The line names the source as a prefix (`2001:db8:1:2::/64`, `203.0.113.7`) and the limit | An operator should learn that an address is being turned away, and which. A flood from many sources should not learn to fill the log. The per-key pacing of spec 028 #30 is the tool built for exactly that, and the cap of 16 keeps the whole warning to a few lines a minute. |
| 11 | **2026-09-28** — **The order of checks on a sign-in**: the router's body and origin checks (#26, #29) → the over-long email `401` (#26) → the email limiter's reservation (#31 a) → **the source's token** → the gate's place (#31 b) → the comparison. For accepting an invitation: body checks → token length `422` → token lookup `403` (#31 d) → **the source's token** → the gate → the hash. For a password change: session → current and new password lengths → the account's reservation → **the source's token** → the gate. For setup: `TRACEPAD_SETUP=off` / expired `403` (#32) → body checks → token → **the source's token** → the gate | Each check costs more than the one before it, and each refusal gives back what the checks before it reserved. The email limiter stays in front because its refusal is cheaper than the bucket's and says something more specific to the person. |
| 12 | **2026-09-28** — *Amended by #16.* **An untrusted proxy is warned about.** When the server reads a request's source (a password check, a sign-in, `GET /api/v1/system`), a request carrying `X-Forwarded-For` whose peer is a loopback or private address (RFC 1918, `100.64.0.0/10`, `fc00::/7`, `fe80::/10`) and is not trusted logs a `WARN` naming the peer and the setting to change. At most once an hour per peer, for at most 8 peers an hour | Without it, a Docker deployment behind a host proxy (whose peer is the bridge's gateway, not loopback) would put every client in one bucket, and nothing would say why sign-ins fail together. A private peer that forwards is almost always a proxy. A public one that sends the header is a client, and a client inventing headers is not the operator's problem to be told about. |
| 13 | **2026-09-28** — *Amended by #16: `source` only; by #18: a whole address is spelled bare.* **`GET /api/v1/system` reports the limit and your source.** Deployment-wide beside `writer_queue`: `source_limit` with `tracked` (sources held), `capacity` (32,768), `refused` (since the process started) and `trusted_proxies` (the list in effect, as CIDR ranges). Per request: `source`, the source this request resolved to, spelled as the log spells it: an IPv4 address bare, an IPv6 source as its /64 | The one question every proxy deployment has to answer is "what does the server think my address is", and without asking the server it can only be guessed. The gauges follow spec 043 #21's rule: the process's, like the queue. |
| 14 | **2026-09-28** — **Wrong `Bearer` credentials are not limited by source** (spec 028 #31 f and spec 045's out of scope stand) | A key is 32 random characters or more (spec 001 #18), so guessing one is hopeless at any rate. What a flood of wrong keys costs is an indexed read each, under spec 043 #1's deadline, which is the cost of any flood of requests. Limiting it by source would answer a shared NAT's valid exporter `429` because a neighbour's key is wrong, and OTLP exporters retry a `429`. A generic request-rate limit is its own question, with its own measurements. |
| 15 | **2026-09-28** — **Ingest is not limited by source.** The body budget, the span cap, the writer queue's `429` and the read slots (spec 043) stay what bounds it | Ingest is authenticated, and many exporters share one address: a collector fans a whole fleet into one connection, and a Kubernetes cluster leaves through one NAT. The fair share that matters there is per project or per key, not per address, and spec 043 already defers it as fairness between tenants. |
| 16 | **2026-09-28** (found in the first review of PR #127) — **The edges of #1, #8, #12 and #13.** (a) **#1's rationale was too broad.** Loopback stays the default, but not everything that connects over loopback is an HTTP proxy that appends: a TCP relay (a service-mesh sidecar, `ssh -L`, `socat`, stunnel, `kubectl port-forward`) passes a remote client's own `X-Forwarded-For` through, and that client then picks its source. The docs say so beside the setting and name the way out: `none`, or the real proxy's address alone. The examples name a gateway address (`172.17.0.1`), not the bridge's range, which would trust every container on it. (b) **The warning of #12 also covers a proxy behind a proxy**: when the walk steps past the trusted hops and stops at an untrusted loopback or private address with more entries to its left, that address is a proxy nobody named — a load balancer in front of a local nginx — and is warned about the same way. A private address with nothing to its left is a client on the network, and is not. (c) **Nothing is warned about under `none`**: the operator chose to trust nobody, and an hourly line telling them to add a proxy is one they could not silence. (d) **`Retry-After` rounds up on every limit**, the email's and the account's included (spec 028 #8, #31): one helper, `waitSeconds`, so a client that waits what it was told is not turned away for waiting 0.4 s too little. (e) **`GET /api/v1/system` reports `source` alone**: `source_limit` goes. (f) **A public route works its client out once**: the address is kept on the request for the session the sign-in opens | (a) The argument "whoever can connect over loopback already runs code on the machine" is true of a local process, not of a relay that carries a remote peer. What such a relay costs is the limit for the clients behind it, which is the server before this spec: the gate still bounds the CPU, and the session list showed a client-chosen address before as well. Changing the default to `none` would instead put every host-proxied deployment into one source, so the default stays and the exception is documented where the setting is. (b) The review's topology — balancer, then nginx on the host — is common, and it put every client into one source with nothing said. The peer check alone could not see it. (c) A warning that cannot be acted on teaches operators to ignore the log. (d) Rounded to the nearest second, a wait of 2.4 s said 2, and the client that obeyed was refused again. (e) The limit's counts are every tenant's sign-in traffic, and the trusted list is the deployment's internal topology, while any project key with `read` can ask for `/system`. `source` is the asker's own, and it is all a proxy check needs. A refusal is in the server's log. (f) Checking a password and then opening a session asked the question twice, and walked the header twice. |
| 17 | **2026-09-28** (found in the second review of PR #127) — *(b) amended by #18.* **Four edges of #1, #4 and #16, and three that stay.** (a) **A proxy behind a proxy is one with an address to its left** (amends #16 b): an empty or garbled entry there is the client's own header passed along — `X-Forwarded-For: unknown` from a private client behind the local nginx — and is not warned about. (b) **A range of every address refuses to start**: `0.0.0.0/0`, `::/0`, and `::ffff:0:0/96`, which #1's reading of mapped ranges turns into `0.0.0.0/0`. Narrower ranges, public ones included, are the operator's to name. (c) **The setting is kept as its text**: `Config.TrustedProxies` is the string, read by one function, rather than a nil list for the default and an empty one for `none`. A `Config` built by hand whose value does not parse trusts nobody. (d) **A NAT64 address is its own source** (amends #4): in `64:ff9b::/96` and `64:ff9b:1::/48` the whole address is the source. (e) **`X-Forwarded-Proto` and `X-Forwarded-Host` keep being read from any peer** (spec 028 #23, #30): the list governs `X-Forwarded-For` alone. (f) **On a sign-in the source's token stays after the account's read**, where #11 put it. (g) **A peer address that does not parse is stored as no address**, and such requests share one source | (a) Warned about, the garbled case told the operator to trust a client, which would then name its own source. (b) Trusting every peer lets every client name its own source, so the limit is off, and a line at `INFO` was all that said so. Cloudflare's own ranges are as wide as /13 and /29, so the line is drawn at "everything" and nowhere narrower. (c) Any copy, reset or round trip of a `Config` that turned the empty list into nil turned `none` into `loopback` without a sound. Failing closed on a bad value means a mistake costs clients their own buckets, not the limit. (d) Behind a translator every IPv4 client is `64:ff9b::a.b.c.d`, one /64, and they shared twenty checks. (e) Those two headers describe the sender's own request — the `Secure` flag on its own cookie, the comparison of its own `Origin` — and a page elsewhere cannot make a browser send them, so believing a client about them costs only that client. `X-Forwarded-For` chooses a bucket other people share, which is why it alone waits for a trusted peer. `docs/docker.md` says so beside the four headers. (f) The account read is one indexed lookup beside the request's own HTTP and JSON work. Taking the token before it would mean a second place that charges and a guard against charging twice, for a refusal that is already cheap. (g) The listener is TCP, and net/http always writes `host:port`; only a test double does otherwise. |
| 18 | **2026-09-28** (found in the third review of PR #127) — **The last edges: the peer's header, a silent trusted proxy, a whole address's spelling, and ranges wider than a fleet.** (a) **A peer is warned about only when its header forwards an address** — the rightmost entry parses — as #17 (a) has it for a hop. (b) **A refusal of a source that is itself a trusted proxy says why**: the line carries a `hint` that the proxy sent no client address, so every client behind it is this one source, and that the proxy should append `X-Forwarded-For`. (c) **A whole address is spelled bare** in the log and in `/api/v1/system` — IPv4, and the NAT64 clients of #17 (d) — and a /64 as its prefix, amending #13's wording. (d) **A trusted range may be at most an IPv4 /8 or an IPv6 /16** (amends #17 b): a wider one refuses to start, however the space is split. (e) **The default list is built fresh at each reading**, so no caller can change `loopback` by appending to it. (f) **A token spent by a request that then gave up in the gate's queue is not given back**, as with the full gate's `503` (#8). (g) **The client address is kept on the request only on the public routes with a body**, the ones that check a password and then open a session | (a) An empty, `unknown` or garbled header from a LAN client is the client's own, and a warning that called it a proxy told the operator to trust a client. (b) nginx does not append `X-Forwarded-For` unless told (`docs/docker.md` says so), and without the header a trusted proxy is the source for everyone behind it. That looks like a flood from the proxy, and only the log line can say what it really is. A per-request warning is not possible: the operator's own `curl` on the machine arrives the same way. (c) The published contract says an address or a /64, and `/128` was neither. (d) `0.0.0.0/1` and `128.0.0.0/1` got around a check for `/0` alone. An /8 is the largest block one organisation holds, and Cloudflare's widest proxy ranges are a /13 and a /29, so the line costs no real deployment anything. (e) An exported slice was the default, and any package could have changed it. (f) The same reasons as #8: giving the token back would need TAT arithmetic that races with concurrent requests, twenty covers the retries, and a client that walks away from the queue is most often the flood. (g) Only those routes ask twice. A memo at the root would cost an allocation on every request, ingest included, to save a second header walk that no other route makes. |

## API contract

| Where | Status | When | `Retry-After` |
|---|---|---|---|
| `POST /api/v1/auth/login`, `POST /api/v1/setup`, `POST /api/v1/auth/accept-invite`, `PATCH /api/v1/auth/me` with a password | `429` `too many password checks from your network; try again in N seconds` | The source's bucket is empty (#7) | `N` (#8) |

Unchanged: the email limiter's `429` (spec 028 #8), the account's `429` on a
password change (#31 b), the gate's `503` (#31 b) and every other status.

`GET /api/v1/system` gains `source` (#13, #16), declared in `openapi.json` and
`schema.d.ts`.

## Application contract

- `internal/server/source.go`: `(s *Server) clientAddress(*http.Request)
  netip.Addr`, the resolver of Decisions 1–3, over the server's
  `trustedProxies`; `sourceOf(netip.Addr) netip.Prefix` (#4) and
  `sourceText`, the one spelling of a source the log and
  `/api/v1/system` use; `noteUntrustedProxy` (#12). `clientIP` is gone, and
  the session a sign-in opens is stamped with `clientAddress` (#5).
- `internal/server/throttle.go`: `sourceLimiter` (GCRA over a map and a
  min-heap by TAT, #7, #9) and `admitSource`, called first thing in
  `enterPasswordGate`, before `s.passwords.Enter` (#6). It answers the `429`
  itself and logs through `logpace.Keyed{Every: time.Minute, Keys: 16}` (#10).
  The callers' reservations come back through the deferred `cancel` they
  already hold for a full gate (#8).
- `internal/config`: `Config.TrustedProxies` is `TRACEPAD_TRUSTED_PROXIES` as
  written, checked at `Load`; `ParseTrustedProxies` is the one reading of it
  (#17): empty or `loopback` is loopback, `none` an empty list. A bare address
  is its /32 or /128, a range is masked to its network, and an IPv4-mapped
  range is read as IPv4; a range of every address refuses to start.
  `loopback` may stand beside other entries; `none` stands alone.
- `cmd/tracepad/main.go`: the setting in the help text, and one `INFO` line
  at start with the list in effect.

## Config

| Variable | Default | Meaning |
|---|---|---|
| `TRACEPAD_TRUSTED_PROXIES` | `loopback` | Addresses and CIDR ranges whose `X-Forwarded-For` is believed (#1, #2). `none` trusts nothing. |

## Testing

- **The resolver, as a table** (#1–#4): direct peer; a trusted peer with one
  hop; two trusted hops; a spoofed left entry behind a trusted proxy
  (`X-Forwarded-For: 6.6.6.6, 203.0.113.7` from `127.0.0.1` → `203.0.113.7`);
  an untrusted peer with a header (header ignored); two header lines; ports,
  brackets, zones and mapped addresses; `unknown` and garbage mid-list; 33
  entries; IPv6 addresses in one /64 → one source, in two → two; `none`
  ignoring loopback's header.
- **GCRA with a fake clock** (#7): 20 admitted at one instant and the 21st
  refused with `Retry-After: 3`; one admitted every 3 s indefinitely; a
  refused request does not move TAT.
- **Eviction** (#9): expired sources leave before any live one; past the
  ceiling, the least-debt source goes; a source in debt survives 32,768
  sources that owe less; memory stays under the ceiling after a million
  distinct sources.
- **The #31 flood, replayed** (the acceptance test): from one source, logins
  with a fresh email each, at a rate that filled the gate in spec 028's
  audit. The gate's `Spent` grows by at most 20 plus one per 3 s, and a
  sign-in from a second source succeeds throughout, with no `503`.
- **Order** (#11): a sign-in refused by the source keeps its email's count
  (the reservation came back); an email locked out spends no token; a made-up
  invitation token spends no token; a password change refused by the source
  keeps the account's count.
- **Behind a proxy, end to end**: the server on a real listener, reached from
  loopback (trusted by default) with `X-Forwarded-For` as a proxy appends it.
  Two clients with different forwarded addresses have separate buckets, and
  the session list shows the client's address, not the invented left entry.
- **`/api/v1/system`** (#13, #16): `source` is the forwarded client's /64,
  and nothing of the limit's is reported.
- **The third review round** (#18): a peer whose header holds no address is
  not called a proxy; a refusal of a trusted proxy's own address says the
  proxy forwarded nobody, and a client's refusal does not; a NAT64 source is
  spelled bare; ranges wider than /8 or /16, split or whole, refuse to start
  and the widest real ones pass.
- **The second review round** (#17): a private client whose own header left
  an empty or garbled entry to its left is not called a proxy; a range of
  every address refuses to start; `none`, copied, is still `none`, and a
  setting built by hand that does not parse trusts nobody; two NAT64
  clients are two sources.
- **The review round** (#16): a balancer in front of the trusted local proxy
  is warned about, and a private client behind it is not; nothing is said
  under `none`; `Retry-After` rounds up; a public route works its client out
  once.
- **The Playwright suite** signs in hundreds of times, all from `127.0.0.1`.
  Its harness gives every sign-in, invitation and setup an address of its own
  in `X-Forwarded-For` (198.18.0.0/15, RFC 2544), through the loopback proxy
  the server trusts by default. Without that, the suite would be one source
  and would meet the limit a flood meets.
- **The warning** (#12): a private untrusted peer with the header logs once
  an hour; a public one never does.
- **Config**: an unparsable entry refuses to start, naming it; `loopback`,
  `none`, bare addresses and CIDRs parse.

## Edge cases

- **Behind a proxy nobody named**: every client is one source and shares one
  bucket, twenty at once and twenty a minute. A small team never notices. A
  flood from anywhere turns everyone's sign-ins into `429`, which is the
  failure spec 028 #31 records, now with a status that says why and a warning
  that says what to set.
- **Docker**: with `-p 127.0.0.1:4318:4318` and a proxy on the host, the peer
  is the bridge gateway (`172.17.0.1`, or Docker Desktop's VM address), not
  loopback. The docs give the setting, and #12 catches the deployment that
  missed it.
- **One attacker, many sources**: a /48 of IPv6 is 65,536 /64s, and a
  botnet is many IPv4 addresses. Each gets its own twenty. The limit bounds
  one source, not all of them together, and the gate stays what bounds all
  of them (#31's stated price, now paid only by a distributed flood).
- **A proxy that overwrites instead of appending** (Caddy's default for an
  untrusted client): the header holds one entry, the client as the proxy saw
  it, which is exactly the rightmost entry the walk wants.
- **A trusted proxy that sends no header**: the source is the proxy.
- **A restart** forgets every bucket, as the email limiter does: a speed bump,
  never a lockout that outlives the process.
- **The monotonic clock** keeps a wall-clock step from refilling or draining
  every bucket at once.
- **An IPv4 client over an IPv6 listener** is read as IPv4 (#2), so it is not
  its own /64 of `::ffff:0:0/96`.
- **Teredo** (`2001::/32`) carries an IPv4 client too, obfuscated, and its /64
  is one Teredo server's clients. It is left to #4's /64: the protocol is on
  its way out, and a client behind it shares at worst with that server's
  others (#17).

## Docs to touch

- `docs/accounts.md`: "A reverse proxy's rate limit on `/api/v1/auth/` is the
  remedy today" is replaced by the limit, its `429`, and what a distributed
  flood still does.
- `docs/accounts.md`: the session list's address, and the variable in the
  configuration table.
- `docs/docker.md`, *Serving over TLS*: a fourth thing a proxy must do
  (append `X-Forwarded-For`, which the nginx block already does), and
  `TRACEPAD_TRUSTED_PROXIES` with the Docker gateway example.
- `docs/api.md`: `source` on `GET /api/v1/system`.
- `openapi.json` (the `429` on setup and accepting an invitation; login and
  the password change already declared one), `schema.d.ts`,
  `cmd/tracepad/main.go` help text.
- Spec 028 #4 and #31 point here.

## Out of scope

- A request-rate limit on every route, by source or by key; wrong credentials
  (#14); ingest (#15); fairness between projects (spec 043).
- A second tier of aggregation (IPv6 /48, IPv4 /24) against a distributed
  flood.
- Reading `Forwarded` or `X-Real-IP`, or a setting that picks the header (#3).
- The numbers as settings (#7).
- An allow-list of sources exempt from the limit.
- Persisting buckets across restarts or sharing them between processes.
