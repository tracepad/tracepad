# Spec 028 — Accounts: people sign in, projects have members

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> The interface signs in with a project key. That was the right answer
> while a tracepad server had one person and one project: the key is
> already in the app's config, the login screen asks for it once. It is
> the wrong answer the moment there is a second project — the key is the
> project, so seeing another one means signing out and finding another
> secret — and a worse one the moment there is a second person: a
> freelancer who should see one project gets the credential that also
> rotates its keys and erases its data, and the only way to take it back
> is to rotate the key under the application too. This spec gives the
> server accounts: a person signs in with an email and a password, is a
> member of some projects with a role, and an owner runs the server.
> Project keys stay what they are for programs — the SDKs, the CLI, the
> MCP server — and stop being something a person pastes into a browser.
> The project switcher that this makes possible is spec 029.

---

## Overview

Deliverables, one branch, a series of PRs in the order below (the last
commit of the last PR flips the status):

- Schema 0017: `accounts`, `memberships`, `account_sessions`,
  `account_tokens` (Decision 2, 3, 5).
- A third kind of caller beside the project key and the admin token: a
  **browser session**, resolved from an `HttpOnly` cookie, scoped to one
  project per request by a header (Decisions 4–6).
- Every route in the route table carries an explicit **policy**; a test
  fails when one does not (Decision 7). The policy is the whole of the
  permission matrix (Decision 3).
- `POST /api/v1/auth/login`, `…/logout`, `GET /api/v1/auth/me`,
  `PATCH /api/v1/auth/me`, the session list, the setup and invite
  endpoints (Decisions 8–11).
- Account management for owners and for the admin token: `GET/POST
  /api/v1/accounts`, `GET/PATCH/DELETE /api/v1/accounts/{id}`, `POST
  …/invite`, `PUT/DELETE …/projects/{project_id}`, and `GET
  /api/v1/projects/{id}/members` (Decision 12).
- First run prints a setup link instead of a pre-authed key link; the
  interface shows the setup screen until an owner exists (Decision 9).
- The interface: `/login` asks for email and password, `/setup` and
  `/invite` set a password from a link, the sidebar carries an account
  menu, Settings is split into *Project* (by role) and *Server* (owners
  only: projects, accounts). The admin-token input leaves the interface
  (Decisions 13–15).
- CLI `tracepad accounts …` over the same endpoints with the admin token
  (Decision 16).
- The application-line ceiling rises to 20,000 (Decision 17).
- `docs/accounts.md` (new), `docs/admin.md`, `docs/ui.md`,
  `docs/quickstart.md`, `docs/docker.md`, `docs/cli.md`, `openapi.json`,
  `schema.d.ts`.

Not here: the project switcher and the project in the URL (spec 029);
OAuth/OIDC providers; sending email of any kind; self-registration; a
third project role; per-endpoint scopes on project keys; accounts on
the ingest routes; the Langfuse SDK's `Basic` auth for anything but
keys.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-10** — The server has **accounts**: a person with an email, an optional display name, and a password, stored locally (`bcrypt`, cost 12). The email is the sign-in name, unique case-insensitively, never verified and never written to — it is an identifier the owner types, not an address the server uses. A password is at least 10 characters and at most 128 with no other rule. The word *user* is taken: spec 023's users are the traced application's end users, and `/api/v1/users` is their listing — so the person who signs in is an **account** everywhere: tables, routes, CLI, docs | The product's own design said "no users, no roles, no sessions", and that held while one person ran one project. It stops holding at the first helper, freelancer or partner who should see one project and not the others — a real situation for the solo developer this is built for. Local password accounts are the version of that with no external service, working in `docker-compose` and offline; providers can come later behind the same account row. `bcrypt` is the boring choice and `golang.org/x/crypto` is the one new dependency. Ten characters and a length cap is the modern password rule (composition rules make passwords worse, and 128 is where `bcrypt` stops reading anyway). |
| 2 | **2026-09-10** — Two levels of standing. An **owner** is a flag on the account: every project, every management action, the accounts themselves. Everyone else is a **member** of specific projects with a role, `viewer` or `editor` (`memberships(account_id, project_id, role)`). Owners have no membership rows — the flag is the membership. There can be several owners; the last enabled owner cannot be demoted, disabled or deleted (`409`) | A partner needs everything the founder has, so owner is a role rather than a person. Two project roles are the fewest that separate "look and annotate" from "change the project", which is the line a helper sits on; a third role (annotator) would be a row in the matrix, the docs and every endpoint's tests for a distinction the two already draw (Decision 3). "Last owner" is the one invariant a server needs to never be locked out of itself. |
| 3 | **2026-09-10** — The permission matrix, by route policy (Decision 7). `public`: no credential (`GET /api/v1`, `openapi.json`, `/health`, the setup and login endpoints). `ingest`: **project key only** (`POST /v1/traces`, `/api/public/ingestion`, `/api/public/otel/v1/traces`, `POST /api/v1/scores` from a program — see below). `member`: a project key, or a session whose account is an owner or has any role in the project: every data-plane `GET`, plus the annotation writes — `POST`/`DELETE /api/v1/scores`, queue `next`/`complete`/`skip`/`reopen`. `editor`: a project key, or an owner or `editor` session: prompts (versions, labels, delete), datasets, items, runs, score configs, queues (create, replace, delete, add items), `PATCH` the project's retention, keys (list, mint, revoke), user-data erasure. `owner`: the admin token, or an owner session: create, delete, restore, **rename** a project, list all projects, everything under `/api/v1/accounts`, `GET /api/v1/projects/{id}/members`. The admin token keeps exactly the powers spec 005 #11 gave it plus the account routes, and still reaches no data-plane route | The matrix is the spec's contract, so it is one table here and one column in the code, not prose per endpoint. Annotation is a viewer's job — the helper this is for scores traces and works a queue — so it sits with reading, not editing; `POST /api/v1/scores` is therefore `member`, not `ingest`, and a key still writes it as before. Renaming moves from "admin token" (spec 007 #12) to `owner` for the same reason it needed the token: a project's name is the echo every destructive confirmation is typed against. The admin token stays off the data plane (spec 005 #11, 006 #13): the reason it was refused there — a browser holding the key to everything — is exactly what accounts replace, so nothing needs it there now. |
| 4 | **2026-09-10** — A browser session is a cookie: `tracepad_session`, `HttpOnly`, `SameSite=Lax`, `Path=/`, `Secure` when the request arrived over TLS or carries `X-Forwarded-Proto: https`. The value is 32 random bytes, base64url; the server stores `sha256(value)` as the row id in `account_sessions(id, account_id, created_at, last_seen_at, expires_at, user_agent, ip)`. A session lasts `TRACEPAD_SESSION_DAYS` (default 30) and **slides**: a request seen more than a day after `last_seen_at` moves `expires_at` forward and rewrites the cookie's `Max-Age`. Expired rows are swept by the retention sweeper. Sign-out deletes the row; a password change deletes every other row of the account | `HttpOnly` is the reason to use a cookie at all: the key in `localStorage` (spec 006 #8) was readable by any script on the page, and a session that lasts a month should not be. A hashed row means a read of the database does not hand out live sessions, the same reason keys are stored as hashes (spec 001 #8). Sliding thirty days is "sign in about once", which is what a tool you open every day should ask; the once-a-day write keeps the writer out of every read. `Secure` follows the actual scheme rather than a flag, because the common deployments are plain localhost and a TLS proxy, and a flag defaults wrong for one of them. |
| 5 | **2026-09-10** — Cross-site request forgery is closed by **origin**: a request authenticated by cookie with a method other than `GET`, `HEAD` or `OPTIONS` must carry an `Origin` (or, failing that, `Referer`) whose host equals the request's `Host`; otherwise `403 "cross-origin request refused"`. Requests carrying an `Authorization` header are exempt, and the header wins when both are present: an explicit credential beats an ambient one | `SameSite=Lax` already stops the classic cross-site `POST`, but it is a browser default rather than a guarantee, and a login cookie that authorises `DELETE /api/v1/projects/{id}` deserves a check the server makes itself. Comparing `Origin` to `Host` is stateless, needs no token in the page, and is exactly what the SPA on the same origin passes for free. Header-over-cookie is the rule that keeps the CLI's behaviour untouched when someone runs it from a browser-adjacent tool. |
| 6 | **2026-09-10** — A session names the project it is asking about with **`X-Tracepad-Project: <id>`** on every route whose policy is `member` or `editor`. Missing → `400 "a session must name the project: X-Tracepad-Project"`; a project the account cannot reach → `403 "not a member of this project"`; a soft-deleted project → `404` unless the caller is an owner and the route is one of spec 005 #10's two. A project key ignores the header (it is its own project); the `{id}` routes under `/api/v1/projects` take the project from the path as they do now | A key carries its project; a session does not, so something must. A header, rather than a query parameter, because `?project=` would ride on every listing URL beside the filters, be copied into shared links, and collide with the CLI's `--project`, which means something else. The interface adds one header in one place (the client), and spec 029 will put the id in the page URL and read it back from there. 403 rather than 404 for a project you are not in: ids are random, so there is nothing to enumerate, and "not a member" is the message that tells a person what to ask the owner for. |
| 7 | **2026-09-10** — The route table (`routes.go`) gains a **policy column** — `public`, `ingest`, `member`, `editor`, `owner` — and one `authorize` reads it: header credential first (key → project; admin token → deployment), else cookie → account, then the policy decides. A parity test walks the table and fails for a route with no policy, and a permission test hits every non-public route as each of the six callers (no credential, key, admin token, viewer, editor, owner) and asserts the matrix of Decision 3 status by status. The three `authorize`/`authenticate`/`target` paths in `admin.go` and `otlp.go` collapse into this one | A matrix that lives in prose is checked by nobody; a column in the table the router is built from is checked by the compiler for presence and by one test for meaning. Six callers times every route is a few hundred cheap requests, and it is the test that turns "a viewer cannot revoke keys" from a belief into a fact — the spec 004 #33 isolation test, applied to roles. |
| 8 | **2026-09-10** — `POST /api/v1/auth/login {email, password}` → `200 {account}` and the cookie; wrong email, wrong password, disabled account and unset password all answer the same `401 "wrong email or password"`. Five failures for one email inside fifteen minutes → `429` with `Retry-After`, counted in memory. `POST /api/v1/auth/logout` deletes the session and clears the cookie. `GET /api/v1/auth/me` → `{account: {id, email, name, owner}, projects: [{id, name, role}]}` — for an owner every live project with role `owner`, for a member their memberships — or `401`. `PATCH /api/v1/auth/me {name?, password?: {current, new}}`; a password change needs the current one and signs every other session out. `GET /api/v1/auth/sessions` lists the account's sessions (`current: true` on the caller's), `DELETE /api/v1/auth/sessions` ends every session but the current one | One `401` text for every failure is the rule that keeps the login form from enumerating accounts. Fifteen minutes and five tries is enough to stop a dictionary run without a person ever seeing it. `me` is the one call the SPA makes on load; the project list rides in it because the shell needs both at once and spec 029's picker needs nothing more. "Sign out everywhere" is what a person does after a laptop goes missing, and the reason sessions are rows. |
| 9 | **2026-09-10** — **Setup.** While no enabled owner exists, the server generates a 32-byte setup token at each start, keeps it in memory only, and prints `http://<host>/setup#token=…` where the pre-authed key link used to be; `GET /api/v1/setup` → `{required: true}` is the one thing the interface can learn without a credential, and every other screen redirects to `/setup`. `POST /api/v1/setup {token, email, password, name?}` creates the first owner, signs it in, and answers `403` to a wrong token or once an owner exists. `GET /api/v1/setup` then answers `{required: false}` and the token is gone | This is spec 006 #8's zero-friction first contact with the key replaced by an account: the one moment the operator is provably at the console is the moment the server prints a link, and a link is what a browser needs. In memory and per start, so a token from a log file yesterday opens nothing today, and a restart is the recovery if the link was lost. No environment variable for the owner's password: a password in `docker-compose.yml` is the thing this spec exists to stop pasting. |
| 10 | **2026-09-10** — **Invitations.** An owner creates an account without a password (`POST /api/v1/accounts {email, name?, owner?, memberships?: [{project_id, role}]}`) and receives `{account, invite_url}` once: `http://<host>/invite#token=…`, a single-use token (`account_tokens(id = sha256, account_id, expires_at)`) valid seven days. `POST /api/v1/auth/accept-invite {token, password}` sets the password, deletes the token, signs the person in. `POST /api/v1/accounts/{id}/invite` mints a fresh one — this is also the **password reset**: it ends the account's sessions, and until the link is used the old password still works so an owner cannot lock someone out by mistake. The server hands the link to the owner; carrying it to the person is the owner's job | No mail, so the link is the invitation, and an owner pasting a link into a chat is a smaller ask than an operator configuring SMTP. A token that sets a password means the password never passes through the owner's hands, which is the whole difference between an invitation and "here is your password". Reset-by-invite reuses one path for two needs and keeps the old password live until the new one lands, because the failure mode of a reset is somebody locked out on a Friday. |
| 11 | **2026-09-10** — The `host` in the printed setup and invite links is the request's `Host` for an invite (the owner is on the interface already, so the interface's address is the right one) and the listen address via `displayHost` for setup (there is no request yet). `TRACEPAD_URL`, when set, wins for both | The right host is the one the person will use, and the server can only guess it; the request's `Host` is the best guess there is for a link an owner is about to copy, and the operator's `TRACEPAD_URL` is the answer when the guess is wrong (behind a proxy). |
| 12 | **2026-09-10** — Account management: `GET /api/v1/accounts` (every account with `owner`, `disabled`, `last_login_at`, `pending: true` while no password is set, and the memberships), `GET /api/v1/accounts/{id}`, `PATCH /api/v1/accounts/{id} {name?, owner?, disabled?}`, `DELETE /api/v1/accounts/{id}` — a dry run until `?confirm=` echoes the **email** (spec 005 #8), deleting the memberships, sessions and tokens with it; `PUT /api/v1/accounts/{id}/projects/{project_id} {role}` and `DELETE …` for one membership; `GET /api/v1/projects/{id}/members` for the project settings screen. Disabling an account ends its sessions at once. Setting `owner: true` on an account with memberships deletes them (Decision 2); setting it `false` leaves none, so the account sees nothing until it is given projects | Owners manage people from the account side (a person and their projects) and read from the project side (who can see this) — the two questions the two screens ask. The echo for deletion is the email, the one thing about an account a person means. Disabling is the reversible way to take access away today; deleting is for people who are gone, and it takes nothing else with it — scores do not name their author and nothing references an account. |
| 13 | **2026-09-10** — The interface: `/login` is an email and a password; `/setup` and `/invite` read `#token=` from the fragment, strip it (spec 006 #8's rule), and show name, email (setup only) and a password with a confirmation. No key is ever accepted at the login form and nothing is kept in `localStorage` about the credential: `auth.svelte.ts` becomes "have we a session" answered by `me`, `401` on any request sends the person to `/login?next=`, `{required: true}` from setup sends them to `/setup`. The API client sends `credentials: 'same-origin'` and the project header on every request; the current project is the first of `me.projects` by name, remembered per account in `localStorage` (`tracepad.project.<account id>`) so a reload keeps it. Where the sidebar shows the project name, spec 029 puts the switcher; until then it is the name | No transitional key login (there are no installed servers to carry): two ways in would be two login screens, two test suites and two docs for a week. The fragment rule is kept because it is right: a link in a chat must not keep the secret in history. "First project by name" is the least surprising default for a screen that cannot yet pick, and remembering it per account keeps two people on one machine from swapping each other's project. |
| 14 | **2026-09-10** — The sidebar gains an **account menu** at the bottom (display name or email, the role of the current project as a caption): *Account*, *Sign out*. `/settings` splits into three tabs. **Project** (the current project): name (rename for owners, read-only with the reason for everyone else), retention, keys, danger zone — each card rendered for editors and owners and shown read-only with a one-line "your role in this project is viewer" for viewers, so a viewer sees what the project is set to and knows why the buttons are gone. **Account** (everyone): display name, change password (current + new + confirm), the session list with "sign out everywhere". **Server** (owners only; the tab is absent, and the route redirects, for anyone else): the projects table exactly as spec 007's Administration had it (create with the keys-once dialog, delete with echo, restore, soft-deleted with purge dates), and an **Accounts** table — email, name, owner, status (`pending` / `active` / `disabled`), last login, projects with roles — with *Invite* (a dialog: email, name, owner or a project list with a role per row; the invite link shown once with a copy button), *Edit* (the same fields, plus disable, plus *new invite link* for a reset), *Delete* (dry-run card with the email echo). The admin-token input and `admin.svelte.ts` are removed | Settings was one page because one credential unlocked all of it; three tabs are three audiences. Showing a viewer the project's cards read-only rather than hiding them is the spec 007 #12 principle (a disabled control with the reason beats a missing one). The Accounts table is the page an owner opens to answer "who can see what", so the projects and roles are on the row, not behind a click. The admin-token input goes because nothing in the interface needs it any more (Decision 3), and a credential the screens do not use is a credential the screens should not hold. |
| 15 | **2026-09-10** — The interface hides what the role cannot do, and the server refuses it anyway: for a viewer the annotation controls stay and the prompt editor, dataset editor, score-config and queue management, and the project cards' buttons are not rendered; a `403` that reaches the interface regardless (a role changed under an open tab) renders the server's message verbatim in the card or dialog it came from, exactly as spec 007's admin `403`s do. The role comes from `me.projects` and a rename or role change in Settings calls `me` again | Hiding is for the person; refusing is for the security. The interface reads one list once and gates on it, so a role is a property of the shell, not a check in each screen; the `403` path exists because that list can be stale, and the server's text is the one that knows why. |
| 16 | **2026-09-10** — CLI: `tracepad accounts ls`, `accounts create <email> [--name] [--owner] [--project <id>:<role> …]` (prints the invite link), `accounts show <id|email>`, `accounts invite <id|email>` (a new link; the reset), `accounts set --owner/--no-owner --name --disable/--enable`, `accounts grant <id|email> <project> <role>`, `accounts revoke <id|email> <project>`, `accounts rm <id|email>` (dry run, `--confirm <email>`), all over the endpoints above with the admin token (`--key` or `TRACEPAD_API_KEY`), the way `projects …` already works. **`TRACEPAD_ADMIN_TOKEN` is the documented recovery** when every owner's password is lost: set it, run `tracepad accounts invite <email>`, open the link | The CLI stays a client of the API (spec 004): no command reaches into the database. The lost-owner case needs *some* out-of-band credential and the admin token is already that; a server with no token configured and no owner who can sign in restarts to get a setup link only if there is no owner at all (Decision 9), so the doc says plainly: keep the token somewhere, or keep a second owner. |
| 17 | **2026-09-10** — The application-line ceiling rises from 18,500 to **20,000**. `main` after spec 027's tails is 18,141; the login rewrite, the setup and invite screens, the account menu, the Account tab, the Accounts table with its invite and edit dialogs and the membership editor, the role gating and the client methods are estimated at 1,300–1,500 lines, less the 150 the admin-token section returns, and one review cycle needs room | The raise rule of spec 015 #9: `main` plus the measured estimate plus a cycle. This is the largest interface addition since the annotation queues and the estimate is per screen above; the PR reports `make ui-lines` before and after, by file. Spec 029 will state its own. |
| 18 | **2026-09-10** — The four tables' timestamps are Unix **nanoseconds**, not the milliseconds the schema block below says. The block's own comment gives the reason for the correction — "like every timestamp here" — and every INTEGER timestamp in this schema since 0002 is nanoseconds: `traces.ingested_at`, `annotation_items.added_at`, `projects.deleted_at`. The schema block below is left as written and this row is the correction | A second unit would need a second formatter at the API boundary, and `formatTime` — the one function that turns a stored instant into the RFC 3339 the whole surface answers with — takes nanoseconds. One unit off by a factor of a million, in a column a sweeper compares against `time.Now()`, is a class of bug worth not having; the retention window and the session window are now the same arithmetic. |
| 19 | **2026-09-10** — Three routes Decision 3 does not assign a policy to one-for-one. `GET /api/v1/projects` is **`member`**: it is the route that answers *each* caller with what it can reach — every project for the admin token, its own for a key, `me.projects` for a session — so "list all projects" is what the token gets from it rather than what it takes to call it. `PATCH /api/v1/projects/{id}` is **`editor`**, because retention is a project's own settings, with the rename checked for `owner` inside the handler (the body says which of the two it is). `GET /api/v1/system` is **`member`**: its counters are the asking project's own (spec 004 #10). And the routes under `/api/v1/projects` take their project from the path or have none, so a session sends no `X-Tracepad-Project` on any of them, the listing and the create included | The policy column is what a caller must *hold*, and these three are routes whose answer, not whose admission, varies by caller. Giving the listing `owner` would take it away from the project key that reads it today and break the interface a PR before its replacement lands; giving the `PATCH` `owner` would mean a project's own editor could not shorten its retention, which is the one setting an editor is for. |
| 20 | **2026-09-10** — Two things the invitation rules leave open. A fresh link **voids the previous one** for that account (`InviteMint` deletes the account's tokens before inserting), and the setup of a first owner whose email already belongs to an account **promotes that row** rather than inserting a second | A reset whose earlier link stayed live is a reset that closed nothing, which is the one thing a reset is for. And the email column is unique, so "a server whose only owner is disabled lets the setup create a new owner" (edge cases) has to say what happens when the operator types the disabled owner's own address — the answer that matches the intent is to hand that account the password and the flag back. |
| 21 | **2026-09-10** — **Spec 005 #10 is superseded**: the credential that can delete a project is the one that restores it, and a project key reaches neither. That decision let a soft-deleted project's own key call `restore`, and this one takes it away — leaving `GET /api/v1/projects/{id}` as the one route that still answers such a key with anything, so that whoever is about to restore can see what they are restoring and until when (the listing answers it with an empty one, as before) | #10's reason was that a token-less deployment which deleted its only project would have no credential able to undo it. Deleting already needed the admin token, so that deployment could not have got into the state in the first place; and with accounts there is always an owner, because the last one cannot stand down (Decision 2). What is left of the old rule is the asymmetry it created — a leaked `sk` that cannot destroy a project but can resurrect one — and an application credential should move a project in neither direction. |
| 22 | **2026-09-10** — An **owner is counted only when they can sign in**: the flag on, not disabled, *and* a password set. Decision 2's "last owner" and Decision 9's setup rule both measure that, and a `pending` owner — invited and never accepted — counts as no owner exactly as a disabled one does. So: a server with no owner who can sign in prints a setup link, and its setup may promote an existing row (Decision 20); and the last owner who can sign in may not be demoted, disabled or deleted even when a pending owner exists | Found in review. Half a rule is worse than none: the invariant exists so that somebody can always open the door, and an account with no password cannot. The reachable version is ordinary — an owner invites a second owner, the invitation is never opened, the first stands down — and it ends with a server nobody can sign in to that *also* prints no setup link, because it believes it has an owner. Without `TRACEPAD_ADMIN_TOKEN` that deployment is unrecoverable. |
| 23 | **2026-09-10** — The cross-site check of Decision 5 compares the `Origin`'s (or `Referer`'s) host against **any of three**: the request's `Host`, the first value of `X-Forwarded-Host`, and the host of `TRACEPAD_URL` when it is set. Anything else is still `403 "cross-origin request refused"` | Found in review. `Host` alone is right for a direct connection and wrong behind a proxy that rewrites it: the browser sends the `Origin` the person typed, the server compares it to the internal name it was reached by, and every cookie write — sign-out, a score, accepting an invitation — answers 403 while a project key goes on working, which reads as a broken interface rather than as a misconfiguration. The other two are the operator's own statements of what the address is: `X-Forwarded-Host` is what the proxy says it was asked for, and `TRACEPAD_URL` is what the operator says people type, which this spec already trusts for the links it prints (Decision 11). |
| 24 | **2026-09-10** — `tracepad accounts rm` wears the echo of spec 005 #8 as Decision 16 spells it — `--confirm <email>` — and **not** as the `--yes` every other destructive command takes. At a terminal it prints the server's preview and asks for the email to be typed, the way `projects rm` does; run non-interactively without `--confirm` it stops with the preview on stderr and exit 1. The preview is always asked for first and `--confirm` is checked against the email the server named there, **case-insensitively**, with the server's own spelling going back on the wire. Every command takes the account as `<id|email>`, resolved through the listing, because the API has no find-by-email route and the CLI has no logic of its own (spec 004 #1) | `--yes` means "send back whatever confirm value the server named", which is right when the thing being deleted was named in the command line and the echo is only ceremony. An account is the case where it is not: `tracepad accounts rm 4b1e…` names an id, and `--yes` would delete whoever that id turned out to be. Naming the person is the whole safety here — the echo is the email precisely because it is the one thing about an account a person means (Decision 12) — so the scripted form makes the script say it. The comparison is the client's and case-insensitive because an email is a case-insensitively unique identifier: the server is right to compare its echo exactly, and what the caller has to get right is which account, which case is not — on the one path with nobody there to read a refusal. |
| 25 | **2026-09-14** — The Server tab's projects table is the **way into a project's settings**, refining Decision 14. Each row's name is a link and its primary action is **Settings**, both leading to `/p/{id}/settings/project` — a switch to that project, the same movement as the switcher (spec 029 #6, remembered as any screen under `/p/{id}` is by spec 029 #3); *Delete* is the secondary action beside it, visually subordinate; a soft-deleted row has *Restore* in their place and no *Settings*; and because the table is the server's list while the way in is gated by `me` (spec 029 #4, #13), the table reads `me` again when it lists a live project the shell does not know — one made from the CLI or another owner's session — so the row leads where it says. The Status column stays. On the Project tab an owner has a way back above the cards — *All projects*, to `/p/{id}/settings/server`; a member sees no such line. The caption under the table says that a project's name, retention and keys are changed in its settings (the keys-once sentence stays with the create dialog, which already says it). Nothing on the Server tab edits a project, and the Project tab's cards do not change | Found on the stand: an owner read a project's retention and status in the table and could not see where to change them — nothing led from the row to the Project tab, and the way there (switch in the sidebar, then Project) was not said anywhere. A row that shows a setting should lead to where the setting is changed; the Server tab stays the server's overview rather than growing a second editor for what the Project tab already edits. A two-step drill-down (table → project, project → table) rather than breadcrumbs, because there are only two steps. Delete becomes secondary because the destination is what a row is for; the destructive action is the exception, and it looks like one |
| 26 | **2026-09-26** — **Every public route takes a small, plain body, enforced at the router.** The guard wraps each `public` route whose method can carry a body (everything but `GET` and `HEAD`) before its handler runs: any `Content-Encoding` other than none or `identity` — in any of the request's `Content-Encoding` headers, in any comma-separated token of each — is `415 "this route takes an uncompressed body"`, and the body is capped at **8 KiB** (`http.MaxBytesReader`, so a larger one is cut off, not read) with `413 "request body too large"` past it. Today that is `POST /api/v1/setup`, `/auth/login` and `/auth/accept-invite`, whose handlers read JSON as every other route does. The presigned media PUT is not `public` but **`presigned`**, a policy of its own in the table: no credential in the headers (the endpoint map says `public`, which is what it is to a caller), the token in the URL checked before the body, the body capped at the length the token grants and never decompressed (spec 041 #14). A login whose email is longer than 254 bytes, as sent or lower-cased (the limiter's key), or whose password is longer than 128 answers the Decision 8 `401` at once, without the comparison and without a mark in the limiter. A display name is trimmed and then at most **200 characters** (counted as characters, not bytes) on every route that sets one — setup, `POST /api/v1/accounts`, `PATCH /api/v1/accounts/{id}`, `PATCH /api/v1/auth/me` — `422` past it; the two `PATCH` routes now trim as the two creating routes always did | These routes run before anything knows who is calling, and they shared the reader sized for trace batches: a 20 MiB cap on the wire and gzip expanded to twenty times that, so a few hundred kilobytes of compressed zeros cost the server hundreds of megabytes before the token or password was looked at, and a handful at once was enough to have the process killed. What they carry is a token, an email, a password and a name — at most about 4.7 KiB with every character escaped, a name of astral characters written as surrogate pairs being the worst of it — so 8 KiB is headroom for any client and nothing for an attacker, and no browser or `curl` compresses a request body unasked. At the router rather than in each handler, so that a public route added later is safe without anybody remembering to be careful; the one route with other needs says so in the table, where the policy of every route is read, rather than in a comparison of paths. The over-long login is refused before the limiter because the limiter keeps the email as a map key for fifteen minutes: bounded by count, not by size, it was a way to hold memory the caller sized. No account can have such an email or password, so answering at once tells nobody anything the `401` did not. The name cap is the one string on these routes nothing bounded, and one rule for the field reads better than one for the route that happens to be public; characters because a name is read by people, and a byte limit would give a Cyrillic name half the room of a Latin one — 200 characters is at most 800 bytes, inside the 8 KiB. |
| 27 | **2026-09-26** — *Amended by spec 045 #4.* The keys entry of #3's `editor` row loses "a project key": listing, minting and revoking a project's keys is an owner's or `editor` session's, or the admin token's, and a key on those three routes gets `403`. Every other route of the `editor` and `member` rows still admits a key | Issuing a credential is a person's act; spec 045 #4 has the argument. |
| 28 | **2026-09-26** — In #3's matrix, `GET /api/v1/raw` and `/api/v1/raw/{id}` move from `member` to `editor` (spec 044 #6) | See spec 044 #6. |
| 29 | **2026-09-26** — **The public ways in refuse other origins and non-JSON bodies**, at the router with Decision 26's limits: a request to a public route with a body that carries an `Origin` must name one of this server's hosts (Decision 23's three; `null` is not one) or is `403 "cross-origin request refused"` (Decision 30 adds to the message what to set), and its body must be declared `Content-Type: application/json` or it is `415`. A request with no `Origin` passes the first check, and no other route gains the second (spec 003 #19 stands). `http.CrossOriginProtection` is not used | Login CSRF: a page elsewhere posted an HTML form at `/api/v1/auth/login` with `enctype=text/plain`, whose body is valid JSON, and the response's cookie signed the victim's browser into the attacker's account — after which what they typed went to it. A form can send only three content types without a preflight, and none is JSON, so requiring it closes the form; the `Origin` check closes a `fetch` from a page elsewhere. The CLI, the SDKs and curl send no `Origin` and were never the threat, which is a browser driven by somebody else's page. `CrossOriginProtection` would answer the same question with a second rule: it trusts `Sec-Fetch-Site` and otherwise compares `Origin` to `Host` alone, where the cookie routes have compared it to Decision 23's three hosts since a proxy that rewrites `Host` broke them — one server, one definition of its own origin. |
| 30 | **2026-09-26** — **An origin is a scheme and a host, and it may not downgrade** (amends #23 and #29): where the server knows it is served over TLS — the connection, `X-Forwarded-Proto: https`, or an `https` `TRACEPAD_URL` for that URL's host name, whatever port the origin spells — an `Origin` (or `Referer`) of `http://` on one of its hosts is not its own, on the public routes and on cookie writes alike. For `TRACEPAD_URL`'s host, `http` passes only where it is configured as `http` and the request is not known to have arrived over TLS. Where the server knows nothing about TLS, either scheme is accepted, as before. Any scheme but `http` and `https` is refused, and so is an `http` origin that writes port 443 unless `TRACEPAD_URL` names exactly that address: plain http on the https port is a page an attacker on the path answers. Both checks read the origin the same way: the `Origin`, or the `Referer` when the `Origin` is `null`; a public route checks only a request that carries an `Origin`, so one without passes whatever its `Referer` says (#29). Hosts are compared without the port their scheme implies — `TRACEPAD_URL=https://host:443` and `Host: host:443` both name `https://host` — each under its own scheme, except that a `Host` or `X-Forwarded-Host`, which has none, is read under the origin's; and only the first value of `X-Forwarded-Proto` counts, as with `X-Forwarded-Host`. The refusal, here and on cookie writes, says what to set (`TRACEPAD_URL`, or forward `Host` / `X-Forwarded-Host`), and a `WARN` names the origin sent — its scheme and host only, never a `Referer`'s path or query, where an invitation carries its token — and the hosts it was compared with, each value the sender chose cut to 256 bytes, once a minute for each origin and for at most 64 origins a minute: one source posting on repeat cannot hide the line about the operator's own proxy, and none can fill the log. A source inventing origins by the dozen can still crowd it out, which is why the `403` carries the hint as well | Comparing hosts alone let a page served over plain HTTP on the same name — the redirect page in front of an HTTPS site, rewritten by anyone on the path — post to the sign-in route as if it were the site, and to every cookie write too. Refusing only the downgrade, and only where the server has been told it is HTTPS, is what closes that without breaking the deployment the proxy rule of #23 exists for: a TLS proxy that sets neither header nor `TRACEPAD_URL` sends an `https` origin to a plain-HTTP request, and a strict comparison would refuse every write from the interface behind it. The default port is dropped because a browser never writes it: compared as spelled, a configured `:443` refused every sign-in behind a proxy that rewrites `Host`. An https `TRACEPAD_URL` vouches for its name on every port because `http://host:443` is not another address of the site but a page an on-path attacker answers in plain text; compared as a host with its port, it missed the configured address and passed through `Host: host:443` as if nothing were known about TLS. A forwarded host is read under the origin's scheme because it is the address the browser typed: read under the scheme the request arrived by, a TLS proxy that says nothing about itself and forwards `host:443` turned every sign-in into a `403`. The first `X-Forwarded-Proto` because behind two proxies the header is a list, and a list that is not exactly `https` read as plain HTTP switched the downgrade rule off. |
| 31 | **2026-09-27** — **What an unauthenticated caller can make the server spend on passwords is bounded** (amends #1, #8 and #26). (a) **The login limit is counted before the comparison**: an attempt reserves its place under the limiter's lock as it arrives and counts as a failure until it is known not to be one, so a burst for one email is five comparisons whatever its size; an attempt turned away before the comparison (b) gives its place back, a success clears the email's failures without touching attempts still in flight. (b) **A global gate bounds the `bcrypt` work in flight**: half of `GOMAXPROCS`, at least one and at most four at once, and a queue of four times that behind them, waiting as long as the request does; past it, `503` with `Retry-After: 1` and `"the server is busy checking passwords; try again in a moment"`, and a `WARN` at most once a minute with the count it stands for (the existing `logLimiter`). Every request that spends `bcrypt` takes a place — login (the decoy of #8 included), setup, accepting an invitation, a password change. A current password that was right when checked but whose hash changed before the job wrote — another change landed in between — is `409 "the password was changed while this request was on its way; sign in again"`, not the `403` a wrong one gets. The account is read before a place is taken, so a slow read holds no place. The gate is `store.PasswordGate`, and the two functions that run `bcrypt` (`HashPassword`, `Account.Verify`) take the `PasswordSlot` only it hands out: a caller that forgets the gate does not compile, and one that passes `nil` or a slot it has given back panics. What the gate let through is counted on the gate itself (`Spent`), as is its queue (`Waiting`). A caller that gives up while it waits in the queue is not the gate being full: nothing is logged, counted or written. **A password change runs both of its `bcrypt` steps in the handler**, under one place given back before the job is submitted: the current password against the hash the request read, then the new hash; the job, inside the one writer transaction, only checks that the stored hash is still byte for byte the one checked, and answers `ErrWrongPassword` otherwise. Before, the comparison ran inside that transaction and held the writer — and every ingest behind it — for a quarter of a second. **A wrong current password is a guess**, counted like a login's and reserved before the comparison, but **per account and apart from the login's count by email**: five wrong in fifteen minutes and the next is `429 "too many wrong passwords for this account; try again shortly"` with `Retry-After`; the login is not touched, and a login locked out does not stop a password change. Every place at the gate is taken and given back by one helper that gives it back however the work ends, a panic included, and a login's reservation is given back the same way on any path that neither failed nor succeeded. (c) **The limiter evicts, it does not empty**: past **8192** emails it drops the record counting the fewest attempts (failures in the window plus attempts in flight), and among those the one touched longest ago — records are filed in one list per count, so this is constant time. **The counts are exact**: every failure is also queued, oldest first, and each limiter operation first takes off its record's count every failure that has left the window. (d) **An invitation's token is checked before its password is hashed**: the length first (`422`, no hash), then a read-only lookup of the token (`403`, no hash), then the hash, then the transaction, which checks the token again. (e) **A password is 10 to 72 bytes**, not 128 characters: `422 "a password must be between 10 and 72 bytes; a character outside plain ASCII takes two to four"` on every route that sets one; a login with a longer one is #26's `401` at once. (f) **No per-address limit on wrong `Bearer` credentials**: with spec 001 #18's 32 characters a guess is hopeless at any rate, and a per-IP limit behind a proxy is its own spec (rate limiting, with a trusted-proxy list) | Measured on the audit: fifty logins sent at once for one email were fifty comparisons, because every one read "four so far" before any recorded its failure; 4097 invented emails, one guess each, emptied the map and gave a locked email its five back; and `accept-invite` hashed before it looked at the token, a third of a second of CPU per request carrying no link at all, with no limit of any kind. The gate is the answer to all of them at once for CPU: whatever the route, whoever asks, the machine spends at most four cores' worth on passwords, and ingest and reads keep at least half of it — four at a quarter of a second is sixteen sign-ins a second, which no team reaches by hand. A `503` rather than an unbounded queue, because a queue with no bound only moves the exhaustion from the CPU to the connections; a short one, so that a few people signing in at once wait a second instead of being refused. **The price is stated**: a flood of about sixteen sign-ins a second with a different email each — every one a decoy comparison and a fresh limiter record, so no email's limit trips — keeps the gate full, and every real sign-in, setup, invitation and password change answers `503` for as long as it runs. Before the gate the same flood took every core: signing in slowed to seconds but went through, and ingest and reads slowed with it. This trades the availability of signing in for that of ingest and reads, which are what the server is for. Nothing here can tell the flood from people without knowing where requests come from — a deeper queue is more connections for the flood to hold, and favouring known accounts would say which exist — so what bounds the flood itself is a per-source rate limit behind a list of trusted proxies, which is its own spec. The decoy stays: without it the timing tells whether an email has an account (#8), and inside the gate it costs no more than a real comparison. Eviction takes the fewest attempts first because what a spray buys is exactly the count of the email it pushes out: dropping the whole map gave a locked email its five tries back, dropping the oldest record gave an email four guesses in its four more, and ranking by the count a record was filed under when last touched let a map filled once with records whose failures had long aged out outrank an email with three live ones for ever — three guesses and one new address, repeated, with no lockout at all. With exact counts, pushing out an email with *c* failures takes every other record held at *c* or more inside one window: *c* × 8192 comparisons in fifteen minutes. The gate lets through at most four at a time; at a quarter of a second each that is 14,400 a window, short of the 16,384 that even *c* = 2 needs; where `bcrypt` takes a tenth of a second it is 36,000, and *c* = 4 costs 32,768 — over thirteen minutes of the whole gate, every sign-in on the server refused meanwhile, to win four guesses the window gives back two minutes later. The spray never buys more guesses than waiting. 8192 emails is at most about 4 MB (a 254-byte email and five timestamps apiece). The current password counts as a guess because it is one: without the count, a session held by somebody else guessed as fast as the gate allowed and kept it full, so every sign-in got `503`. Counted apart, because counted with the login a stranger failing five logins every fifteen minutes stopped the person holding the session from rotating the password at exactly the moment they would want to; the session is already proven, and an anonymous spray must not cost its owner that. The price is stated: whoever holds a stolen session gets ten guesses a window — five here, five at the login — instead of five. 72 bytes because it is what `bcrypt` reads, and the library refuses anything longer outright, so the old ceiling of 128 accepted a password as valid and answered `500` on the hash; bytes rather than characters because that is the unit `bcrypt` counts in. A pre-hash (SHA-256, then `bcrypt`) would lift the ceiling and is not worth its sharp edges (a NUL in the digest, a hash format of our own) for passwords past 72 bytes. |
| 32 | **2026-09-27** — **The setup link expires, and can be switched off** (amends #9). The token is good for **24 hours** from when this start minted it; after that `SetupURL` is empty, `POST /api/v1/setup` answers #9's `403` ("restart the server to have it print a new one"), and a restart mints a fresh one as before. The printed text says so: "The link is good for 24 hours, or until this process stops." **`TRACEPAD_SETUP=off`** (default `on`) mints no token and prints no link; `POST /api/v1/setup` answers `403 "setup is turned off on this server (TRACEPAD_SETUP=off); create the first owner with the admin token: TRACEPAD_API_KEY=$TRACEPAD_ADMIN_TOKEN tracepad accounts create <email> --owner"` — a command that runs as printed, since the CLI reads its credential from `TRACEPAD_API_KEY` — before it reads the body (the router's limits on a public body, #26, still come first), and a start with no owner says the same at `INFO` — at `WARN` when there is no admin token either, since then nothing can make one. `GET /api/v1/setup` keeps answering whether an owner is needed and adds `enabled` (false under `off`) and `expired` (setup needed and on, but no link this start printed still works — past its 24 hours, or never minted because an owner could sign in at start). The setup screen says setup is off and names the admin-token command while an owner is needed and setup is off, that the link has expired and a restart prints a new one when it has, and that there is nothing to set up once an owner exists — instead of asking for a link that will never be printed or showing a form the server will refuse. The route guard lets `/invite` through while setup is required: an owner the admin token invited has no password until that link is opened, so the server still needs setting up — and under `off` the link is the only way in, which the guard used to redirect to `/setup` | A deployment that never used the link — its owner made with the admin token, or only the API in use — kept a live way to create an owner in memory, and in every log the start was written to, for as long as the process ran. A day covers "deployed tonight, set up in the morning", and past it a restart is the same recovery #9 already names for a lost link. The switch is for the deployment that should never have one: a link that is not minted cannot leak. |

## Data contract

Schema **0017** — `internal/store/migrations/0017_accounts.sql`:

```sql
CREATE TABLE accounts (
  id            TEXT PRIMARY KEY,                 -- 16 random bytes, hex
  email         TEXT NOT NULL COLLATE NOCASE UNIQUE,
  name          TEXT NOT NULL DEFAULT '',
  password_hash BLOB,                             -- NULL until an invite is accepted
  owner         INTEGER NOT NULL DEFAULT 0,
  disabled      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,                 -- unix ms, like every timestamp here
  last_login_at INTEGER
);

CREATE TABLE memberships (
  account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  role        TEXT NOT NULL CHECK (role IN ('viewer', 'editor')),
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (account_id, project_id)
);
CREATE INDEX idx_memberships_project ON memberships(project_id);

CREATE TABLE account_sessions (
  id           TEXT PRIMARY KEY,                  -- sha256(cookie value), hex
  account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  user_agent   TEXT NOT NULL DEFAULT '',
  ip           TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_account_sessions_account ON account_sessions(account_id);
CREATE INDEX idx_account_sessions_expires ON account_sessions(expires_at);

CREATE TABLE account_tokens (                     -- invitations and resets
  id          TEXT PRIMARY KEY,                   -- sha256(token), hex
  account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL
);
```

A project's purge (spec 005 #9) removes its memberships by cascade; a
soft-deleted project keeps them, so a restore brings the members back.
The setup token is not a row. Expired sessions and tokens are removed by
the sweeper on its usual interval (spec 005), and a lookup ignores an
expired row before the sweeper reaches it.

## API contract

Every endpoint answers JSON; errors are `{error}` as everywhere. Policies
per Decision 3; the table below lists what is new.

| Method, path | Policy | Body → answer |
|---|---|---|
| `GET /api/v1/setup` | public | `{required}` |
| `POST /api/v1/setup` | public | `{token, email, password, name?}` → `201 {account}` + cookie; `403` wrong token or an owner exists; `422` on a bad email, password or name; `413` over 8 KiB; `415` compressed (Decision 26) or not `application/json`; `403 "cross-origin request refused: …"` (naming what to set) for an `Origin` that is not this server's (Decisions 29, 30) |
| `POST /api/v1/auth/login` | public | `{email, password}` → `200 {account}` + cookie; `401`; `429` + `Retry-After`; `413` over 8 KiB; `415` compressed (Decision 26) or not `application/json`; `403 "cross-origin request refused: …"` (naming what to set) for an `Origin` that is not this server's (Decisions 29, 30) |
| `POST /api/v1/auth/accept-invite` | public | `{token, password}` → `200 {account}` + cookie; `403` bad or expired token; `422`; `413` over 8 KiB; `415` compressed (Decision 26) or not `application/json`; `403 "cross-origin request refused: …"` (naming what to set) for an `Origin` that is not this server's (Decisions 29, 30) |
| `POST /api/v1/auth/logout` | session | `204`, cookie cleared. A key or the admin token: `400 "not a session"` |
| `GET /api/v1/auth/me` | session | `{account: {id, email, name, owner}, projects: [{id, name, role}]}` sorted by name |
| `PATCH /api/v1/auth/me` | session | `{name?, password?: {current, new}}` → `{account}`; `403 "wrong current password"`; `422` |
| `GET /api/v1/auth/sessions` | session | `{sessions: [{id, created_at, last_seen_at, expires_at, user_agent, ip, current}]}` |
| `DELETE /api/v1/auth/sessions` | session | `{ended}` — every session but the current |
| `GET /api/v1/accounts` | owner | `{accounts: [{id, email, name, owner, disabled, pending, created_at, last_login_at, projects: [{id, name, role}]}]}` sorted by email |
| `POST /api/v1/accounts` | owner | `{email, name?, owner?, memberships?: [{project_id, role}]}` → `201 {account, invite_url, invite_expires_at}`; `409` email taken; `422` |
| `GET /api/v1/accounts/{id}` | owner | `{account}` with `projects` |
| `PATCH /api/v1/accounts/{id}` | owner | `{name?, owner?, disabled?}` → `{account}`; `409 "the last owner"` |
| `DELETE /api/v1/accounts/{id}` | owner | dry run `{account, would_delete: {memberships, sessions}}`; `?confirm=<email>` → `204`; `409 "the last owner"` |
| `POST /api/v1/accounts/{id}/invite` | owner | `201 {invite_url, invite_expires_at}`; ends the account's sessions |
| `PUT /api/v1/accounts/{id}/projects/{project_id}` | owner | `{role}` → `{membership}`; `409 "an owner has every project"`; `404` unknown project |
| `DELETE /api/v1/accounts/{id}/projects/{project_id}` | owner | `204` |
| `GET /api/v1/projects/{id}/members` | owner | `{members: [{account_id, email, name, role}]}` — owners are not listed; the screen says "and every owner" |

`session` in the policy column is a route that only a cookie reaches;
the six-caller test asserts the key and the token get `400` there. The
existing `GET /api/v1/projects` answers a session with the same rows as
`me.projects` plus the retention fields, `role` included.

`X-Tracepad-Version` keeps stamping every response; `GET /api/v1`'s
endpoint map lists the new routes with their policy word.

## Application contract

**Startup.** `printStartup` prints the OTel and Langfuse lines for a
created project as before; the `# Web interface` block becomes the setup
link when no owner exists (every start, until one does) and the bare
interface URL otherwise. The `#key=` link is gone.

**Login, setup, invite.** Three screens outside the shell. Errors are the
server's message under the form. `/login?next=` returns to the guarded
screen (spec 006's `next` rule, unchanged). `/setup` while `required` is
false, and `/invite` without a token, show a one-line explanation and a
link to `/login`.

**Shell.** On load the app calls `GET /api/v1/setup` and `me` in
parallel: `required` → `/setup`; `401` → `/login`; otherwise the sidebar
renders with the project name and the account menu. Every request from
the client carries the current project's id in `X-Tracepad-Project`
(routes under `/api/v1/projects/{id}` and `/api/v1/auth` do not need it
and the client omits it there).

**Settings.** Three tabs per Decision 14, bits-ui tabs (spec 006 #3),
the active tab in the URL (`/settings/project`, `/settings/account`,
`/settings/server`; `/settings` redirects to the first). Destructive cards
keep spec 007 #5's dry-run → echo shape. The invite dialog shows the link
once with a copy button (`SecretDialog` is the pattern) and the expiry.
The projects table on the Server tab leads into each project's Project
tab, and the Project tab leads an owner back (Decision 25).

**Role gating.** Per Decision 15. The listing screens, the trace detail,
sessions, stats, quality, users and the annotation flow are identical for
every role. Prompts, datasets, evals, score configs and queues render
their editing controls for editors and owners only; the screens
themselves stay reachable.

**Demo data.** The seed that fills a demo server is not in this
repository, but the e2e project fixture gains an owner and a viewer so
the interface tests run as both.

## Testing

- Migration 0017 applies on an empty store and on one with projects and
  keys; a project purge cascades its memberships.
- Password hashing round-trips; a 9-character password is `422`; a
  129-character one is `422`; the hash cost is 12.
- Login: right, wrong password, wrong email, disabled, pending, all `401`
  with one text; the sixth failure inside fifteen minutes is `429`; the
  cookie has `HttpOnly`, `SameSite=Lax`, `Path=/`, and `Secure` exactly
  when the request is TLS or `X-Forwarded-Proto: https`.
- Public bodies (Decision 26): every public route with a body, found by
  walking the table, answers `415` to a compressed body (a second
  `Content-Encoding` header included) and `413` to an endless one having
  read no more than the cap, both from the guard with the handler never
  reached; a login with an over-long email is `401` and leaves no key in
  the limiter; a 201-character name is `422` on all four routes that set
  one, 200 Cyrillic characters pass, and both `PATCH` routes store the name
  trimmed.
- Sessions: a request a day after `last_seen_at` slides `expires_at` and
  re-sets the cookie; an expired row is refused before the sweeper runs
  and removed by it; logout deletes the row; a password change deletes
  the others; "sign out everywhere" keeps the current one.
- Origin: a cookie `POST` with no `Origin` and no `Referer` → `403`; with
  a foreign `Origin` → `403`; with the request's host → passes; a
  Bearer `POST` with a foreign `Origin` passes.
- Project header: missing → `400`; not a member → `403`; a viewer's
  header naming a project they are in → `200`; a soft-deleted project →
  `404`; a key with a header naming another project → the key's own
  project is served (the header is ignored).
- **Policy parity**: every route has a policy; every non-public route as
  each of the six callers matches Decision 3, status by status. Cross-
  project isolation (spec 004 #33) holds for sessions: a member of
  project A with `X-Tracepad-Project: A` never sees a row of B.
- Setup: token printed only while no owner exists; `POST` with a wrong
  token → `403`; a second `POST` after success → `403`; `GET` flips.
- Invitations: a link works once; expired → `403`; a re-invite ends the
  sessions and the old password still logs in until the link is used;
  accepting sets `last_login_at`.
- Accounts: the last owner cannot be demoted, disabled or deleted (`409`)
  and can be once a second owner exists; making an owner drops the
  memberships; deletion is a dry run until the email echoes; disabling
  ends sessions.
- CLI: every `accounts` command against a test server; `--confirm` with
  the wrong email is refused; the usage lines parse.
- Interface (vitest): the login form's error rendering; the guard's three
  outcomes (setup, login, shell); the account menu; the Settings tabs
  per role (server tab absent for a member); role gating on one editing
  screen; the invite dialog shows the link once.
- Interface (Playwright): setup from a printed link → create a viewer
  invite → accept it in a second context → the viewer sees the project's
  traces and no *Server* tab, scores a trace, and gets no editor on a
  prompt; the owner disables the viewer and the viewer's next navigation
  lands on `/login`.
- Interface (Playwright, Decision 25): from the Server tab, *Settings* on
  another project's row lands on `/p/{other}/settings/project` with the
  switcher naming it, and *All projects* there returns to the Server tab;
  a member's Project tab has no *All projects*.
- Chrome DoD on the demo copy: sign in as the seeded owner, the account
  menu, all three Settings tabs, an invite end to end in a private
  window.

## Edge cases

- An email with different case signs in to the same account
  (`COLLATE NOCASE`); the stored spelling is what the owner typed.
- Two owners demote each other at once: the second `PATCH` sees one
  owner left and answers `409`; the check and the write are one
  transaction.
- A disabled owner counts as no owner for "last owner", and for the
  setup rule: a server whose only owner is disabled prints a setup link
  and lets the setup create a new owner.
- A **pending** owner counts as no owner either, for both rules
  (Decision 22): an owner invited and never accepted has no password, so
  they cannot open the door any more than a disabled one can. An owner
  who invites a second owner cannot stand down until that invitation is
  accepted, and a server whose only owner is pending goes on printing a
  setup link — whose setup promotes that very row rather than colliding
  with its email (Decision 20).
- Behind a proxy, the `Origin` the browser sends is the address the
  person typed and `Host` may be the internal one. The cross-site check
  accepts `Host`, `X-Forwarded-Host` or `TRACEPAD_URL`'s host
  (Decision 23), so a deployment that sets either of the last two is not
  a deployment where every cookie write answers `403`.
- An invite for an account that already has a password (a reset) does
  not blank the password; accepting it replaces the hash.
- A session whose account was deleted: the row cascaded, the cookie
  answers `401`, the interface goes to `/login`.
- `me.projects` for an owner excludes soft-deleted projects; the Server
  tab lists them with `?include=deleted` as before.
- The admin token on `/api/v1/auth/*` → `400 "not a session"`; on the
  data plane → `401` as today.
- A `Basic` credential (the Langfuse SDK shape) resolves a key as today
  and never a session.
- The `X-Tracepad-Project` header on a `public` or `owner` route is
  ignored.

## Config additions

| Variable | Default | Meaning |
|---|---|---|
| `TRACEPAD_SESSION_DAYS` | `30` | Browser session lifetime; sliding (Decision 4). Minimum 1. |
| `TRACEPAD_URL` | — | Already exists for the CLI; the server now uses it, when set, as the host of printed setup and invite links (Decision 11). |
| `TRACEPAD_SETUP` | `on` | `off` mints no setup link and refuses `POST /api/v1/setup` (Decision 32). |

`TRACEPAD_ADMIN_TOKEN` keeps its meaning and gains the account routes
(Decision 3, 16).

## Out of scope

- The project switcher and `/p/{id}` routes — spec 029, which starts
  when this one has shipped.
- OAuth, OIDC, SAML, magic links by email, two-factor authentication.
- Self-registration and approval flows.
- Per-key scopes, read-only keys, keys that belong to an account.
- An `annotator` role; per-screen permissions inside a project.
- Audit log of who changed what.
- Recording an account on the scores it writes (`author`) — a later spec
  once accounts exist.
