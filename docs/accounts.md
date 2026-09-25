# Accounts

People sign in; programs use keys.

A project key is a credential an application holds: it is in your SDK's config
and in your CI, it carries exactly one project, and it can do everything to
that project. That is the right shape for an exporter and the wrong one for a
person. A freelancer who should see one project would get the credential that
also rotates its keys and erases its data, and taking it back would mean
rotating the key under the running application.

So the server has **accounts**: an email, a password, and a role in each
project. Keys stay what they are — [ingest](ingest.md), the
[CLI](cli.md), the [MCP server](mcp.md) — and stop being something a person
pastes into a browser.

## The model

Two levels of standing, and no more.

- An **owner** runs the server: every project, every setting, and the accounts
  themselves. It is a flag on the account, not a membership, because an owner
  has every project there is and every project there will be. There can be
  several, and the last enabled one cannot be demoted, disabled or deleted.
- Everyone else is a **member** of specific projects, with a role in each:
  - `viewer` — reads everything in the project and **annotates**: writes and
    retracts scores, and works an [annotation queue](annotation.md). Reviewing
    is what a viewer is for.
  - `editor` — everything a viewer does, plus what a project is made of:
    prompts, datasets, runs, score configs, queues, the retention windows, the
    keys, and erasing a user's data.

An account with no memberships and no owner flag can sign in and sees nothing,
which is the state an invitation leaves it in until somebody gives it a
project.

## The first owner

A fresh server has no accounts, so it prints a link at every start until it has
one:

```
This server has no owner yet. Create the first one — it takes an email and a
password, and nothing is written down anywhere but this database:

  http://localhost:4318/setup#token=…

The link is good until this process stops. Restart to have a new one printed.
```

The token is 32 random bytes, minted per start and held **in memory only**: a
link from yesterday's log file opens nothing today, and if the link is lost,
restarting prints a new one. It rides in the URL fragment, which browsers never
send to the server, so it stays out of the access log and out of the history of
whoever opens it.

Opening the link asks for an email, a password and an optional display name.
The same thing over the API:

```sh
curl -X POST http://localhost:4318/api/v1/setup \
  -H 'Content-Type: application/json' \
  -d '{"token":"…","email":"you@example.com","password":"a long enough one"}'
```

There is deliberately no environment variable for the first owner's password. A
password in `docker-compose.yml` is the thing this exists to stop pasting.

`GET /api/v1/setup` answers `{"required": true}` while there is no owner and
`{"required": false}` afterwards — the one thing the interface can ask without
a credential.

## Signing in

`POST /api/v1/auth/login` takes `{"email", "password"}` and sets a session
cookie:

- `tracepad_session`, `HttpOnly`, `SameSite=Lax`, `Path=/`, and `Secure`
  whenever the request arrived over TLS or through a proxy that said so with
  `X-Forwarded-Proto: https`.
- Thirty days by default (`TRACEPAD_SESSION_DAYS`), **sliding**: a request more
  than a day after the last one moves the expiry forward. Open this daily and
  you sign in about once a month.
- What is stored is `sha256` of the cookie's value, the way a key is stored as
  a hash. A read of the database hands out no live session.

A wrong email, a wrong password, a disabled account and one that has never
accepted its invitation all answer the same `401 wrong email or password`. Any
difference between them would be a way to find out who has an account here.
Five failures for one email inside fifteen minutes and the next answers `429`
with `Retry-After`; the count is in memory, so it forgets on a restart and can
never be the reason somebody cannot sign in tomorrow.

Passwords are `bcrypt` at cost 12, between 10 and 128 characters, with no other
rule: composition rules make passwords worse, and 128 is past where `bcrypt`
stops reading.

Setup, sign-in and accepting an invitation are the three routes that take a
body from anybody, so they take a small one: at most 8 KiB of uncompressed
JSON. A larger body is `413` and a compressed one (`Content-Encoding: gzip`
or any other) is `415`. A display name is trimmed and then at most 200
characters, here and wherever else one is set.

### What a session may ask

A key carries its project. A session does not, so it names one on every request
about a project's data:

```
X-Tracepad-Project: <project id>
```

Missing it is `400`; a project the account is not a member of is `403 not a
member of this project`. The routes under `/api/v1/projects/{id}` take the
project from the path instead, and `/api/v1/auth/*` needs none. A project key
ignores the header entirely — it is its own project.

A request authenticated by cookie with a method other than `GET`, `HEAD` or
`OPTIONS` must carry an `Origin` (or, failing that, a `Referer`) whose host is
one this server answers to; otherwise `403 cross-origin request refused`.
Requests carrying an `Authorization` header are exempt, and that header wins
when both are present: an explicit credential beats an ambient one.

Three hosts count as this server's: the request's own `Host`, the first value
of `X-Forwarded-Host`, and the host of `TRACEPAD_URL`. The last two matter
behind a reverse proxy that rewrites `Host` — the browser sends the address
your people typed, and without them every write from the interface would be
refused while a project key went on working.

### The account's own routes

| | |
|---|---|
| `GET /api/v1/auth/me` | The account and every project it can reach, with the role in each. The one call the interface makes on load. |
| `PATCH /api/v1/auth/me` | Change the display name, the password with `{"password": {"current", "new"}}`, or the preferences with `{"preferences": {…}}` — any of them, together or alone. A password change signs every **other** session out. |
| `GET /api/v1/auth/sessions` | Where this account is signed in, the current one marked, with the user agent and address of each. |
| `DELETE /api/v1/auth/sessions` | Sign out everywhere but here. What you press after a laptop goes missing. |
| `POST /api/v1/auth/logout` | End this session. |

### Preferences

`account.preferences` on `GET /api/v1/auth/me` is how the interface is
arranged for this person: a JSON object the interface owns the shape of and
the server only holds — `{}` until written, at most 16 KiB, and validated as
nothing more than *an object under the size* (`422` otherwise). `PATCH
/api/v1/auth/me {"preferences": {…}}` replaces it **whole**: send back the
object you read, with your change in it, never a fragment to be merged. The
dashboard keeps its block order and hidden set under `dashboard`, keyed by
project id ([ui.md](ui.md#dashboard)); the arrangement follows the person
between browsers because it is on the account. It is the account's own: an
owner's `PATCH /api/v1/accounts/{id}` does not touch it, a project key has
none, and neither the CLI nor MCP reads or writes it.

## Who may do what

Every route in the API carries one of these, and one check reads it. A test
walks the whole table as each of the six kinds of caller, so this is the
contract rather than a description of it.

| Policy | Who gets through |
|---|---|
| `public` | Anyone: `GET /api/v1`, `openapi.json`, `/health`, and the three ways in. |
| `ingest` | A project key, and nothing else. |
| `member` | A project key, or a session whose account is an owner or has any role in the named project. Every read, plus writing and retracting scores and working a queue. |
| `editor` | A project key, or an owner or `editor` session. Prompts, datasets, runs, score configs, queues, retention, keys, user-data erasure. |
| `owner` | `TRACEPAD_ADMIN_TOKEN`, or an owner session. Creating, deleting, restoring and renaming a project; listing every project; everything under `/api/v1/accounts`. |
| `session` | Only a cookie. A key or the admin token is told `not a session`, which is what it is. |

`GET /api/v1` lists every endpoint with its policy word, so an agent can see
what a route would need before it calls it.

The admin token keeps exactly the reach [administration](admin.md) gave it,
plus the account routes. It still reaches no data-plane route: the reason it
was kept off the data plane — a browser holding the key to everything — is
what accounts replace.

## Inviting somebody

An owner creates the account and is handed a link, once:

```sh
curl -X POST http://localhost:4318/api/v1/accounts \
  -H "Authorization: Bearer $TRACEPAD_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"email":"helper@example.com","name":"The Helper",
       "memberships":[{"project_id":"…","role":"viewer"}]}'
```

```json
{
  "account": {"id": "…", "email": "helper@example.com", "pending": true, "…": "…"},
  "invite_url": "http://localhost:4318/invite#token=…",
  "invite_expires_at": "2026-09-17T10:00:00Z",
  "note": "the invitation link is shown only here; only its hash is stored"
}
```

Carrying the link to the person is the owner's job — there is no mail here, and
an owner pasting a link into a chat is a smaller ask than an operator
configuring SMTP. The token is single-use, good for seven days, and stored as a
hash like everything else. It sets the password, which means the password never
passes through the owner's hands: that is the whole difference between an
invitation and "here is your password".

The account reads as `pending` until the link is used and cannot sign in before
then.

### Resetting a password

The same door: `POST /api/v1/accounts/{id}/invite` mints a fresh link and ends
the account's sessions. Any earlier link is void.

The **old password goes on working until the new link is used**. An owner
pressing this by mistake, or a link that never arrives, must not leave somebody
locked out on a Friday.

## Managing accounts

| | |
|---|---|
| `GET /api/v1/accounts` | Every account: standing, `pending`, last login, and its projects with roles. |
| `GET /api/v1/accounts/{id}` | One of them. |
| `PATCH /api/v1/accounts/{id}` | `{"name"}`, `{"owner"}`, `{"disabled"}`. |
| `DELETE /api/v1/accounts/{id}` | A dry run until `?confirm=` echoes the **email**. |
| `POST /api/v1/accounts/{id}/invite` | A fresh link; also the reset. |
| `PUT /api/v1/accounts/{id}/projects/{project_id}` | `{"role": "viewer"\|"editor"}`. |
| `DELETE /api/v1/accounts/{id}/projects/{project_id}` | Take a project away. |
| `GET /api/v1/projects/{id}/members` | The project side: who has a role here. Owners are not listed, because they are not rows — they have every project. |

All of it is also `tracepad accounts …` from a terminal, on the admin token —
`ls`, `show`, `create`, `invite`, `set`, `grant`, `revoke`, `rm`. The
[CLI page](cli.md) has the forms.

Three rules worth knowing before you press something:

- **Disabling** ends the account's sessions at once and keeps its roles, so
  enabling puts everything back. It is how you take access away today.
- **Deleting** is for people who are gone. It takes the memberships, sessions
  and invitations and nothing else — a score does not name its author, and
  nothing else in the database references an account. The echo is the email,
  the one thing about an account a person means.
- **Making somebody an owner** deletes their memberships, because an owner has
  every project. Demoting them leaves none, so they see nothing until they are
  given projects again.

The last owner **who can sign in** cannot be demoted, disabled or deleted:
`409`. It is the one invariant a server needs to never be locked out of itself,
so an owner only counts once they can actually open the door: a disabled owner
counts as none, and so does one who was invited and has not accepted yet. That
means you cannot stand down the moment you invite a successor — only once they
have used their link.

A server with no owner who can sign in prints a setup link again, and that
setup takes over the existing account if you give it the same email.

## In the web interface

All of the above is a screen as well as a `curl`. The setup and invitation
links open the two screens that read `#token=`; `/login` takes the email and
the password; the sidebar's account menu leads to *Account* (`/settings/account`),
where anybody changes their own name and password and ends their other
sessions — an account that is a member of nothing included. A project opens
on its [dashboard](ui.md#dashboard), whose arrangement is kept on the account
(above) and so follows the person from one browser to the next.

Which project a screen is about is in its address — every screen about a
project lives under `/p/{project id}` — and the switcher at the top of the sidebar lists the
projects the account reaches, with the traffic of the last day beside each.
A link without the prefix lands on the project the account last looked at;
an address under a project the account is not a member of is one screen
saying so, not a wall of `403`s. See
[ui.md](ui.md#the-project-in-the-address).

Owners get a **Server** tab in Settings — absent for everybody else, and the
address redirects — with the projects table, each row leading into that
project's own settings, and an **Accounts** table: email,
name, standing, last login, and the projects each account reaches with its
role, plus *Invite*, *Edit* and *Delete*. The invitation link is shown once,
there, with a copy button. See [ui.md](ui.md#settings-and-administration).

The interface hides what a role cannot do and the server refuses it anyway;
what a `viewer` keeps is every listing, every detail screen and the whole
annotation flow, because scoring is what the role is for.

## When every owner's password is lost

Keep `TRACEPAD_ADMIN_TOKEN` somewhere, or keep a second owner.

The token reaches the account routes, so it is the way back in:

```sh
TRACEPAD_ADMIN_TOKEN=… # set on the server, then:
TRACEPAD_API_KEY=$TRACEPAD_ADMIN_TOKEN tracepad accounts invite you@example.com
```

Open the link it prints and set a new password. The same thing with `curl`, if
the binary is not to hand:

```sh
curl -X POST "http://localhost:4318/api/v1/accounts/$ID/invite" \
  -H "Authorization: Bearer $TRACEPAD_ADMIN_TOKEN"
```

A server with **no** owner at all prints a setup link at every start, so that
case recovers by restarting. A server with an owner who cannot sign in and no
admin token configured does not — which is why the sentence above is the first
one in this section.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `TRACEPAD_SESSION_DAYS` | `30` | How long a browser session lasts. It slides, so this is "how long since you last opened it", not "how long since you signed in". Minimum 1. |
| `TRACEPAD_URL` | — | The address your people actually use. The server prints setup and invitation links at its own guess otherwise — the listen address, or the request's `Host` — which is wrong behind a proxy, and its host is one of the three the cross-site check accepts. |
| `TRACEPAD_ADMIN_TOKEN` | — | Unchanged from [administration](admin.md), and now also the account routes. |

Expired sessions and invitations are removed by the
[retention sweeper](retention.md) on its usual pass. A session that has run out
stops working the moment it does, not when the sweeper next runs.
