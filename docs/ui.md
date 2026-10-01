# Web interface

A browser client of the read API, served by the same binary on the same port.
It has no endpoints and no logic of its own: everything it shows is reachable
with `curl`, and a feature that is not in [api.md](api.md) does not exist.

## Opening it

The interface is at the server's root — `http://localhost:4318/` on the
default listen address.

It always authenticates. There is no localhost bypass: behind any local
reverse proxy every request looks like loopback, which would silently turn
authentication off for the internet.

A person signs in with an **email and a password** — an
[account](accounts.md), not a project key. A key is what a program holds;
the interface stops being a place to paste one.

- **First run.** The server has no owner yet, so it prints a **setup link**
  next to the connection strings (`http://localhost:4318/setup#token=…`) and
  every screen redirects to `/setup` until somebody uses it. The token
  travels in the URL fragment, which browsers never send to a server; the
  screen reads it and takes it back out of the address bar. It is minted
  afresh at every start while nobody can sign in, so a link from yesterday's
  log opens nothing — and a restart is the recovery if it was lost.
- **An invitation.** An owner creates an account and is handed a link
  (`/invite#token=…`) once, to carry to the person themselves; the link sets
  their password and signs them in. The same link, minted again, is how a
  lost password is replaced.
- **Later.** `/login` asks for the email and the password. Every way of
  failing — wrong email, wrong password, disabled, invited and never
  accepted — is one refusal with one sentence, because any difference
  between them is a way to find out who has an account here.

The session is an `HttpOnly` cookie the browser holds and no script can read.
It lasts about a month and renews itself as you use it; a `401` on any
request returns to the login screen with where you were. Nothing about the
credential is kept in `localStorage`. Signing out ends the session on the
server, and *Account → Where you are signed in* ends the others.

The admin token is not a credential this interface takes at all: an owner
reaches everything it used to unlock (see [admin.md](admin.md)).

### The project in the address

Every screen is about one project, and the address says which: everything
inside the shell lives under **`/p/{project id}`** — `/p/{id}/dashboard`,
`/p/{id}/traces`, `/p/{id}/settings/server`, and so on. A link therefore
names its project, which is what a link shared between two people has needed
since there were two people; it carries the id rather than the name, because a
name can be renamed and a link that breaks on a rename is worse than a hex
segment. `/login`, `/setup` and `/invite` are the three screens outside the
shell and keep their paths. One screen inside the shell is about the person
rather than a project and lives bare: the Account tab, at `/settings/account`
— it is yours whether or not you reach a project.

A **bare path** — `/`, or `/traces?status=error` from a bookmark, a chat, or
the pre-authed link the server prints — redirects to the same path and query
under the **remembered project**: the one this account last looked at in this
browser, or the first by name until it has looked at one. Every link written
before the prefix keeps working. `/` and `/p/{id}` open the project's
[dashboard](#dashboard); so does the login form with nowhere else to return
to, and so do the setup and invitation links once the password is set. An
account that reaches no project is sent to `/p`, which says so — and, for an
owner, offers to create one.

The **switcher** at the top of the sidebar, where the project name is, lists
the projects the account can reach with each one's traces of the last 24 hours
beside it — is it alive — and, over eight projects, a box that narrows the
list. Choosing another project keeps the section you are in (and the Settings
tab) and keeps the filters, the time window, the search and the page size;
what it drops is what belonged to the old project: the open trace, prompt or
queue, the page cursor and the peek panel; from a screen that is under no
project it lands on the dashboard. An owner has *New project* at the bottom of
the menu, which opens the same dialog the Server tab uses and lands on the
new project's dashboard, where the setup instructions are.

An address under a project the account cannot reach — mistyped, deleted, or
one you are not a member of — renders one screen saying so, with the switcher
open beside it, and sends no request for it; the interface does not say which
of the three it is, because the server does not either.

## What a role sees

The sidebar carries an **account menu** at the bottom: the display name or
the email, the role in the project on screen as a caption, and the two things
the menu is for — the Account tab and signing out.

Every screen is reachable for every role, and the dashboard — its Customize
mode included, since the arrangement is the account's — the listings, the
trace detail, sessions, quality, users and the whole annotation flow are
identical.
Scoring is a `viewer`'s job — that is what the role is for. What a viewer is
not offered is the writing: the prompt editor and its labels, the dataset
item editor and the archive, deleting a prompt, a dataset, a queue, a run or
a trace, the score configs' create and edit, and *Add to queue* — filling a
review programme is queue management, and queue management is an `editor`'s.

Hiding is for the person; refusing is for the security. The server answers
every one of those `403` regardless (see [accounts.md](accounts.md)), and a
`403` that reaches the interface anyway — a role changed under an open tab —
is rendered in the server's own words in the card or dialog it came from.

## Screens

**Dashboard** — the project's front page, first in the sidebar and what
`/p/{id}` opens: how much the project ran, what it cost, whether it broke and
whether the scores moved, at a glance. See [Dashboard](#dashboard).

**Traces** — the listing. One row per trace, mapping onto
`GET /api/v1/traces`: time, name, environment, user, session, cost, tokens,
latency, TTFT and how many observations failed. TTFT sits beside latency because the
two answer the same question from opposite ends — how long the whole run took,
and how long somebody waited before anything appeared. The filter bar offers
exactly the filters the endpoint accepts (`q`, `from`, `to`, `environment`,
`user_id`, `session_id`, `name`, `tag`, `status`, `min_cost`, `min_tokens`,
`release`, `version`, `type`, `prompt`) — a test reads `openapi.json` and fails
if the two ever disagree. The bar underneath turns the pages. **Min tokens**
is input plus output tokens, a whole number: a fraction typed into it, or
carried by a hand-edited link, is dropped rather than sent
([api.md](api.md#tokens)). The **Tokens** column shows the same number — input
plus output, compact (`950`, `12.4k`, `3.1M`) — and a dash where the trace
carried neither; its tooltip lists every class the trace reported, exact, so
reasoning and cache tokens are read there and are never added to the figure
(spec 049 #1, #3, #9). The Sessions and Users tables have the same column
over their own sums, and fold it under the id on a narrow screen with the cost.

Three of them — **environment**, **release** and **name** — are checkbox lists
rather than boxes to type in. The values come from
[`GET /api/v1/facets`](api.md#filter-values) for the window in view, read when
the panel opens and again when the window moves under it, and each carries how
many traces of that window have it: `prod 1` beside `production 4656` is a
typo, and nothing but the count says so. Ticking two means *either*, which
travels as the comma form the API takes (`?environment=production,staging`).
Above eight values the list gets a box that narrows it by substring, and past a
hundred it says how many it left out. A value that arrived in a link and is not
in the list — `?environment=canary`, from before canary was retired — is shown
checked at the top without a count, because a filter that cannot be seen cannot
be undone.

A chip names up to two values in full and counts beyond that
(*Environment: 3 values*), with all of them in its tooltip: the chip row is
read at a glance, and a glance holds two names.

There is no release column: release is a filter, and it is shown in the header
of a trace rather than in every row of the listing.

**Search** — the box on the bar, beside the time window. It searches what the
observations carried, not their labels: prompts, answers, metadata, names and
error messages. It commits on Enter and on leaving the box, never on a
keystroke, and it lives in the URL like every other filter.

A row that matched grows a second line under it: which observation and field
the hit was in, and the text around it, with the words of the query marked.
Clicking such a row opens the panel **on that observation** rather than at the
top of the trace — which is the whole reason the row says where it matched.
The matching rules are the API's, and worth knowing at the box: words rather
than substrings (`err*` for a prefix), `"quoted words"` must be adjacent, and
all the words have to occur in the same field of the same observation. See
[api.md](api.md#search).

**Live** re-reads the newest page every five seconds and shows it. It is off
by default, pauses while the tab is hidden, and only runs on the newest page
(see [Turning pages](#turning-pages)). Same caveat as `tracepad tail`: traces
are ordered by their own timestamps, so a span that arrives late appears
where it belongs rather than at the top — which on a full page means it can
push the oldest row of that page off it.

When a project has no traces at all the listing says so in one line and
points at the dashboard, where the exporter settings are; with a filter or a
search set it says what matched nothing instead.

**Sessions** — one row per session over `GET /api/v1/sessions`: last seen,
id, how many traces, how many of them failed, cost, tokens, first seen. The filters
are the endpoint's four (`from`, `to`, `environment`, `user_id`) — the
environment as the same checkbox list the Traces panel offers, and the user id
as a box, because a user id is not a short finite set — and a row
opens the session: its totals over its traces, and a trace opens from there.
Every number counts traces, which is what a session is a collection of.

The session id in the Traces table, in a trace's peek panel and in a trace's
header is a link here, after the row's own link and the user id. Inside a
session — its page, its panel — it stays plain text: it names the session
being read.

## Scores

A judgement about a trace, an observation or a session is shown where its
target is read, and can be written from there. It is
[scores.md](scores.md) on screen: every chip came out of
`GET /api/v1/scores`, and *Score* is one `POST` — the same write an eval
harness makes.

**The block** — a *Scores* row on the trace header (wherever a trace is read:
the page, the peek panel, a session's drill-down, a run item's), on the
observation panel between the payloads and the metadata, and on the session
header. One chip per score: the name, the value, where it came from and how
long ago, newest first. Expanding a chip shows the comment — and the whole of
a long text score — with *Edit* and *Delete* under it.

The value is rendered by its type: a number to three significant digits with
the config's range as its tooltip, a boolean as *yes* or *no*, a category as
itself, a text score cut at 120 characters. The source is `metadata.source`
when the writer set one — `web` for this screen, whatever the SDK or the
widget wrote otherwise — and `api` when it did not.

Beside the source, the [author](scores.md#who-wrote-a-score): *by Ada* for an
account, *key nightly-judge* for a program's key. The source says what kind of
judgement it is, the author whose. Its tooltip is the account's email for an
editor or an owner; a viewer reads the name alone, and an account that never
set one is *a member*. An author who is gone — removed from the project,
disabled, deleted, a revoked key — is muted, with the reason in the tooltip:
the judgement stands. A score from before the server recorded authors shows
none.

The trace header carries the scores of the *trace*; a score that names an
observation belongs to that observation's panel, and the header says how many
went there. That count is what opening the panels will find, so it can be
acted on. The tree badges each observation with how many it has, so "which step
was graded" is answered before any panel is opened. A score naming an
observation this trace does not carry — a late span, a wrong id — is shown in
the header marked *unknown observation*, because no panel would ever show it,
and it is not in the count for the same reason.

**Score** opens a dialog. The name is a select over the project's [score
configs](scores.md#score-configs), and the control under it is the one the
config dictates: a number input bounded by its `min`/`max`, two buttons for a
boolean, a select over the categories, a text area. *other…* takes a free name
and asks what kind of value it carries — a project that declared no configs
must still be able to score. Saving posts one score stamped
`metadata: {"source": "web"}` and no `timestamp`, so it is received now.

**Edit** is the same dialog over an existing score, re-posted with its id:
the score API's correction, which replaces the row rather than adding a second one.
It is offered on every score, not only the ones written here — a judge's
verdict overruled by a person is the review the eval loop exists for. What the
dialog does not show, it resends as it was: the score's `metadata`, its
`timestamp`, and what it is about — so an edit invents neither a new author nor
a new event time, and it never moves a score off the observation or the session
it grades.

**Delete** asks once, naming the score and its value, and then calls
`DELETE /api/v1/scores/{id}`. There is no name to type back: a score is one
row that writing the same id again puts straight back.

## Prompts

*Prompts* is a top-level section, between *Users* and *Evals*: a prompt is a
production artefact — what the application ships — rather than an eval noun.
It is [prompts.md](prompts.md) on screen, and it writes through exactly the
endpoints described there.

**Prompts** — one row per name over `GET /api/v1/prompts`: name, type, the
latest version, the labels as chips with the version each points at
(`production` first, then alphabetical), and when the name last gained a
version. A row opens the prompt. An empty listing shows the `push` that fills
it and the SDK line that reads it back.

**Prompt** — the versions down the left, the version the URL names on the
right. The header carries the name, the type, where each label points, and
*New version*, *Diff* and *Delete*. `?version=V` picks a version — absent
means the latest — and the version list is the shared listing, so a name with
hundreds of versions pages like everything else. The view is the body as it
is: role-labelled blocks for a chat prompt, one block for a text one — prose,
not an escaped JSON string — with `config` as a document, and two links that
answer where it ran: *Traces with v7* and *Traces with any version*, which are
the `prompt=` filter [the traces listing takes](api.md#filters). A `?version=` the name
does not have says so, with the version list still beside it.

**Labels** — the control lives on the version being read: the labels on it as
chips with a ×, and *Add label…*, which offers the name's other labels or
takes a new one. Attaching a label that points nowhere is immediate — a new
label is a note. Moving one that points at another version, and removing one,
ask first, and the question names the move: *production: v6 → v7*, which is
what a rollback reads before confirming. A move writes no version: promoting
and rolling back are the same one-line operation the API describes.

**Diff** — `?diff=A..B` replaces the version view with the patch
`GET /api/v1/prompts/{name}/diff` returns, painted per line: additions,
removals, hunk headers. The interface computes no diff, so the screen, the CLI
and an agent all read the same one. The two selects are number inputs bounded
by the latest version, not menus of every version. `?diff=3..3` says
*identical*; a name with one version says there is no diff yet.

**Writing a version** — *New prompt* takes a name and a type; *New version*
opens a copy of the version on screen, because an edit *is* a new version. The
bodies are plain text areas — a prompt has no syntax to lint — one for a text
prompt, one per message for a chat one, each with a role beside it and *add*,
*remove*, *up*, *down*. The role is a menu of the roles the runtimes name —
`system`, `user`, `assistant`, `developer`, `tool`, `model` — plus *Custom…*,
which opens a field for any other string, because the API stores whatever role
you give it; a message that arrives with a role outside the menu opens in that
field. An added message alternates: after a `user` an `assistant`, after
anything else a `user`. Nothing requires a `system` message — a body that is
one `user` turn is a prompt like any other. `config` is the same JSON surface
every payload uses. *Save* posts one version and lands on it. The gate is the
server's own rules mirrored at the fields — the name's grammar, a non-empty
body, a role and content per message, a config that parses, and `latest`
refused as a label — so that a `400` naming
one of them is not how you find out; the server still decides.

**Delete** takes the name whole: every version and every label, in one
transaction. It wears the ceremony every act with a blast radius wears here —
the server's dry run on screen, with what it would take and what it would not,
and the name typed back. Traces that ran the prompt keep the name and version
they recorded, so the trace filter goes on answering for it.

## Evals

The sidebar's *Evals* section is the eval nouns of [datasets.md](datasets.md)
on screen — **Datasets**, **Runs** and **Score configs** — with the review
queues of [annotation.md](annotation.md) beside them ([Queues](#queues)) and
what they all produce, over time, at the end ([Quality](#quality)).
Every table is the shared listing (pages in the URL, the bar underneath),
every row opens in the
peek panel, and every payload is the same surface as a trace's. Nothing is
computed here that the API did not send: a mean, a verdict, a delta are the
server's, so the screen, the CLI and the MCP tools cannot disagree about
whether a change made it better.

**Datasets** — one row per dataset over `GET /api/v1/datasets`: name,
description, version, items, runs, updated. A row opens the dataset. *New
dataset* takes a name and a sentence and nothing else — that is the whole
envelope — and lands on the empty dataset; a name the project already has is
a link, not a silent replacement of its description.

**Dataset** — the envelope in the header, and two tabs in the URL
(`?tab=items`, `?tab=runs`). *Items* lists the cases at the version in force,
oldest first in first-appearance order: position, id, the first characters of
the input and the expected output as compact JSON, and the version the row
was written at. The **version** control in the header is a number, not a
menu — a dataset edited in CI has hundreds — and rewrites the tab to
`?version=V`; an older version is read-only and says so, because an edit *is*
a new version at the head. A row opens the item whole: the three bodies as
documents, where the case was cut from (a link, offered rather than promised —
the trace may be gone), and every row of the item's history, each linking to
the dataset at that version. An item reached from an older version that has
since been archived says so. *Runs* is the dataset's runs, newest first, with
the checkboxes below.

**Editing a case** — *New item* in the dataset header, *Edit* in an item's
panel, or *Add to dataset* on any observation (below) all land on the same
page: three editors — *Input*, *Expected output*, *Metadata* — over the
payload surface with the parse error in place. *Save* opens once the input is
a document and neither other pane is a broken one; an empty pane is a field
left out, not a syntax error. What a save reports is the store's own answer:
*saved as version V*, or *unchanged* when the write changed nothing, because
a write that changes nothing writes nothing and only the server can say so.
Saving again edits the same item — an id re-posted is an edit — and every
earlier version keeps the row it had. Writes land at the head, so *New item*,
*Edit* and *Archive* are offered only while the head is the version on
screen; *Archive* asks once and removes the case from the current version
while leaving it readable at every earlier one. *Delete* on the dataset is
the one act here with a blast radius, and wears the same ceremony as the rest
of the interface: the server's dry run on screen — items, runs, and the
traces those runs were pinning, which are released rather than deleted — and
the dataset's name typed back.

**Runs** — every dataset's runs over `GET /api/v1/runs`, newest first, with
the two filters the endpoint takes (`dataset`, `status`) and the capped count.
There is no coverage column: a page of runs is for choosing one, and the
listing's rows carry no summary. Two **checkboxes** and *Compare* are the
second way into a comparison: the button is disabled — with the reason as its
tooltip — unless exactly two runs of one dataset are ticked, so the mistake
the server would refuse stays a tooltip rather than becoming a page. Runs are
not created here: a run is opened by the harness that will stamp its traces,
and the empty listing shows the request that opens one.

**Run** — the header names the run, links `dataset@version` to the items tab
at that version, shows the status the harness said (and, beside `running`,
how long it has been open), and offers *Compare with…*, a select of the
dataset's other runs, and *Delete* — which asks once and names what it does:
the run's bookkeeping goes, and the traces it was keeping stop being pinned
and live as long as retention says. The cards are the summary `GET /api/v1/runs/{id}`
computes: coverage (items, covered, missing, unknown traces), traffic (count,
attempts max, failed, cost, p50/p95 — exact over the run's traces), the
**scores** table (type and direction, count, mean, range or a distribution
bar), what actually ran (models, prompts — derived from the traces, not
declared), and the harness's metadata. Below them the run's items from
`GET /api/v1/runs/{id}/items`: position, id, how many attempts, and every
attempt's value per score name in order — not a mean, which is the server's
to take. *Unknown* appends the traces that named a case the dataset does not
have at this version. A row opens the case: the expected output, and each
attempt with its trace, latency, cost, errors, scores and the output its root
observation produced. A cut payload loads whole from where it lives — the
item from its dataset, the output from `/observations/{id}/io`. An attempt's
trace opens **one level deeper in the same panel**, and *‹ Item* in the
header returns, the way a session panel drills into a trace.

While a run is `running` the page re-reads the summary and the newest page of
items every five seconds and stops when the harness closes it; a hidden tab
does not poll. A run with no trace yet shows the two attributes to stamp and
the request that closes it.

**Compare** — `/runs/{a}/compare/{b}` renders `GET /api/v1/runs/{a}/compare/{b}`
and nothing else: both runs with what they ran, the traffic side by side with
the cost delta, the metadata keys the two disagree about, and per score name
both aggregates, the delta and how many cases improved, regressed or stayed —
*changed* rather than *improved* for a name without a direction. The cases
follow, each with its two values and a verdict chip per name. **Changed only**
(`?changed=1`) hides the rows whose every verdict is `same` on the page — the
counts in the header are about the whole pair and do not move — and **Swap**
is the same comparison the other way round. A row opens the case: the
expected output, and *Output A* / *Output B* — the root output of the newest
trace each run made at the case, with a link to all of its attempts when there
were several — beside the per-name pair. The comparison is reachable from a
run's *Compare with…* and from two ticked checkboxes on any runs table.

**Score configs** — name, type, direction, what the name admits (bounds or
categories), description. The form writes one: it offers the vocabularies the
API takes and applies the rules between them before the round trip — a
direction is required for a numeric or boolean name and refused for a
categorical or text one, bounds belong to a number, categories to a
categorical name — with the server as the oracle either way. It `PUT`s the
whole config, so the same form creates and edits and a re-save of an
unchanged config writes nothing. *Remove* takes the binding only: the scores
already posted under that name stay exactly as they are.

**Adding a case from a trace** — every observation panel has *Add to
dataset*. It opens the editor with that observation's input as the case and
its output as what a good answer looks like — both fetched whole, never from
a preview, because a cut document saved as a test case is a wrong test case —
and with the trace and observation recorded as where the case came from. The
dataset is chosen on the editor page, since the reader arrives from a trace
with no dataset in mind. The output arrives as a starting point: a golden
case is usually what the model said with a correction, which is why the
gesture lands in an editor rather than in a *saved* toast.

Every empty state teaches the CLI: a project with no datasets shows the whole
loop in six lines, a dataset with no items the `push` that fills it, a run
with no traces the attributes that link them.

## Queues

The fourth child of *Evals* is the review programmes of
[annotation.md](annotation.md): a named list of traces somebody decided
deserve a human verdict, and the score names that verdict is made of.

**Queues** — one row per queue over `GET /api/v1/queues`, whole and in name
order (a project has as many queues as review programmes, so there is nothing
to page): name, description, the scores as chips, and a bar of how far it has
got — completed filled, skipped hatched. A skip is progress that produced no
verdict, which is why it is not the same colour. *New queue* takes a name and
a multi-select over the project's [score configs](#evals), in the order a
reviewer will be asked for them; a project that has declared none is sent to
declare one, because a queue may only name a config that exists.

**Queue** — the header carries the description, the score chips and the same
progress bar, plus *Start annotating* and *Delete*. Under it, the items in the
order they were added, which is the order they are worked in: position,
target, status, who finished with it, when, and the skip reason. `?status=`
narrows it, and *Mine* (`?account=me`) keeps the items you hold or finished.
Who finished with an item is named as a score's author is — muted, with the
reason in the tooltip, when they are no longer in the project. A row opens **the trace** in the peek panel — that is what an item
points at — and an item that names an observation opens on it. *Reopen* puts a
completed or skipped item back in the queue; *Remove* takes it out of the list
and touches no score. *Delete* is the echo ceremony every destructive act
wears, and its note says the part that matters: the scores written while
annotating stay.

**The desk** (`/queues/{name}/annotate`) is the point of the section: read,
judge, next, with nothing to navigate. Who is reviewing is who is signed in:
the server holds the claim and the completion by the account, the desk asks
nobody for a name, and the header shows yours without offering to change it,
because reviewing as somebody else is signing in as them. Every score it
writes has the account as its [author](scores.md#who-wrote-a-score). Then the
trace on the left, exactly as every other screen shows it, and on the right
one control per score the queue asks for, built by the same rule the *Score*
dialog uses: the config decides the field.

The form arrives **prefilled** from the scores already on the target, whoever
wrote them — a judge's verdict is a verdict, and the reviewer confirms or
edits it rather than repeating it. *Complete & next* posts what changed and
then completes the item; the server checks the shape against the stored scores
and a refusal marks the controls it names. *Skip…* asks for a reason. *Later*
releases the claim and leaves, so nobody waits out its ten minutes.

An item is claimed for ten minutes when it is handed out, so two people at one
desk do not review one trace twice; a reload resumes the same item rather than
moving the reviewer mid-verdict, and `?item=` in the URL says which. When
there is nothing to take, the screen says whether the queue is finished or
whether the rest is claimed by somebody else.

**Adding to a queue** — two gestures, each where the choice is made. A trace
header and an observation panel carry *Add to queue*: pick the programme, and
the answer says *added* or *already in the queue*. The traces listing carries
*Add to queue…* beside the filter bar: it names how many traces the filters
match before the call, queues the newest of them up to the endpoint's cap of
1,000, and above that cap it is disabled with the reason — a queue is a list
somebody has to work through.

## Deleting traces

The one way a trace leaves the store by hand ([admin.md](admin.md#deleting-traces)),
offered at the two surfaces where the choice is made and to editors only,
like *Add to queue* beside which each of them sits.

**On the trace header** — *Delete…* on a trace, in the peek panel or on the
full page. The dialog opens on the server's dry run — the observations,
scores and queue items that go with the trace, the eval runs that would lose
it, and the note that the raw OTLP body stays — with the trace id already
filled in as the echo: it is on screen, and typing thirty-two hex characters
back is a ritual, not a check. *Delete this trace* is the act. From the peek
panel the panel closes and the listing under it re-reads; from the trace's
own page the page leaves for the listing; from a session, the session is
re-read one trace shorter; from the desk, the item went with the trace and
the desk moves on.

**On the traces listing** — *Delete…* beside *Add to queue…*. The dialog
names what it is about before it asks for anything: the listing's own count,
the active filters as the same chips the bar shows, the search text, and the
moment the set is closed at — the filter's own `to`, or, when it has none,
**the moment the dialog opened**, said in so many words, because an empty
filter is *every trace before now* and the wording has to make that plain.
The dry run counts the match exactly where the listing's count stops at a
thousand. The echo is the project's name; then the deletion runs in rounds
of a thousand, a line saying *2,000 of 12,000 deleted*, and *Stop* beside it
finishes the round in flight and leaves the rest — every round is a complete,
consistent act, and a person who sees the number and changes their mind must
not need to close the tab. The listing re-reads when it is over.

## Quality

The fifth child of *Evals* is what the evals and the reviewers have been
saying, over time: [quality.md](quality.md) in a screen. It sits with the eval
nouns rather than with the traffic on the dashboard, because quality is what
evals produce; the dashboard's Quality block draws the six busiest of these
cards and leads here.

The filter bar is the dashboard's: the time window (`?from=&to=`, the one this
browser [remembers](#the-remembered-window) or the last 30 days), the
environment box, and the hourly/daily choice (`?group_by=`).

**Quality** (`/quality`) is a card per score name in the window, over one
request. A card is the name, its data type, how many scores it holds, and a
small chart of what the name says: the mean for a numeric one, the rate of true
for a boolean one, and one line per category — as a share of the bucket — for a
categorical one. A numeric name whose [score config](#evals) pins a `min` and a
`max` is drawn against that axis rather than against its own spread, so a stable
score does not read as a cliff; a name without a config still shows, because the
trend is real whether or not anybody declared it. Configured names come first,
in the configs' order. The whole card is a link.

The endpoint returns the fifty busiest names, so a project that files more than
that gets "N of M score names" in the header and a line under the cards saying
how many are not shown — a shorter grid that looked complete would be the one
thing worse than a truncated one
([quality.md](quality.md#two-ceilings-on-an-answer)).

**One score** (`/quality?name=X`) is the dashboard's composition for one
name: the trend full width — with the minimum and the maximum as two fainter
lines behind a numeric mean — a count chart beside it, and three breakdown
tables under them: by model, by environment, by release. Each row is the key,
how many scores, and the same summary the card draws.

The model breakdown is the question a judge score on generations exists to
answer, and it is empty for a score written on the trace: a trace has no model.
That is not a bug in the screen — the API says so in its `targets` field, and
[quality.md](quality.md#what-is-counted-and-what-is-not) says why.

A window with no scores in it says so and shows the SDK line that starts
recording one; a `?name=` the window does not hold gets the same answer for that
name rather than a not-found page.

## Turning pages

Every listing sits on a bar: how many rows per page, what is on screen
against what matches, and four ways to move — « newest, ‹ previous, next ›,
oldest ». The page is in the URL (`?limit=&cursor=&direction=`), so a reload
comes back to it and a link carries it.

There are no page numbers, and the reason is the same one that makes the
listing fast. Pagination is a **keyset**: a page is found by seeking to a
cursor, so page four hundred costs what page one costs — and both ends are
just a direction to read the index in, which is why « and » are as cheap as ‹
and ›. An ordinal ("page 12 of 40") would need `OFFSET`, which counts and
discards every row it skips, plus a full count on every filter change. In a
listing sorted by time an ordinal is not an address anyway: nobody wants page
40, they want 26 August — and the time range control answers that.

The count beside the rows is capped at 1000 and reads `1000+` past that. It
follows the filters, not the page, so turning a page does not re-count. The
cap bounds the number, not always the work behind it — see
[Counting](api.md#counting) for where that costs something — but it is always
a fraction of what the listing on the same filter already costs.

**Live** only runs on the newest page. Anywhere else the toggle is paused and
says why: re-reading the newest page would replace the page you navigated to.

## The peek panel

A row does not navigate away from its listing. It opens in a panel that
slides in from the right, over the listing, which stays where it was — with
its filters, the pages you already loaded and the position you scrolled to.
Reading a listing is a loop, and this is the loop:

- **Click a row** to open it, and click another to swap the panel over to it;
  the row it is showing stays lit. **⌘/Ctrl-click** still opens the full page
  in a new tab, because the row is a real link to it. Dragging across a cell
  selects its text as it would anywhere else — an id in a listing is
  something you copy into a terminal — and a click that ended a selection
  opens nothing.
- **`k`** and **`j`**, or the two chevrons in the panel's header, walk the
  rows — the buttons carry the key they answer to. On the last row of a page
  `j` turns the page and opens the first row of the next one, so a scan does
  not stop at a boundary that is an artefact of paging; both keys dim only at
  the ends of the whole listing. If the panel is open on a row the page no
  longer holds — a link somebody sent, a live tick that pushed it out of the
  newest page — the keys still mean what they say, and go to the nearest row
  that way. A letter typed into a filter field is a letter, not a shortcut.
- **⤢** opens what the panel is showing as a full page — `/traces/{id}` or
  `/sessions/{id}`, the selected observation included. That is the link to
  send somebody.
- **Escape**, or **✕**, closes it. So does the browser's Back button: what
  the panel shows is in the URL (`?peek=`), so a reload comes back to it.

A session's panel goes one level deeper: a trace in its table replaces the
panel's body, and **‹ Session** in the header returns to the session. On a
phone the panel covers the screen, which is the whole reason it is there —
closing it costs nothing, where a page navigation would read the listing
again.

## Dashboard

The project's front page (`/p/{id}/dashboard`; the old `/stats` address
redirects here, query and all). It is the screen that was *Stats* with the
row a front page needs above it: figures with their movement, the charts and
breakdowns, the quality cards, when the last trace arrived, and — for a
project with no traces yet — the instructions for the first one.

The **summary row**: four tiles for the window on screen — *Traces*, *Cost*,
*Errors* (failed traces as a share of all) and *Latency* (the p95) — each with
its change against the **previous window**, the same length ending where this
one begins. Traces and cost move as a signed percentage, the error rate in
points, the p95 as a signed duration, with an arrow; cost, errors and latency
colour an increase as worse, traces neither, because more traffic is the
denominator and not a verdict. The previous figure is in the tile's tooltip.
A tile whose figure is absent — nothing priced, nothing timed, no traces —
shows a dash and no change. A change against an absent previous figure reads
*new* — a window whose previous window predates the project, or predates
what the rollup still keeps — and so do traces and cost against a previous of
zero, which no percentage can be taken against; the error rate and the p95
are differences and read the difference, so errors appearing where there were
none is `+3 pt`, marked worse. The figures are two
requests for `GET /api/v1/stats?group_by=total`, this window's and the
previous one's ([api.md](api.md#statistics)); the subtraction is the only
arithmetic the screen does.

**Last trace** in the header — *Last trace 4 min ago*, the instant in the
tooltip — is the newest row of the trace listing with the environment filter
and no window, because the one thing a window of statistics cannot say is
whether anything is arriving now. *No traces yet* when the listing is empty.
It is read with the rest of the page and by *Refresh*, never polled: the
Traces live toggle is the one poller in the app.

The **charts** — traces, cost, tokens (input, output and cache read as three
lines, with reasoning and cache write in the legend, hidden until their entry
is clicked, so the drawn lines stay the ones that do not double-count; where
they are the only lines with data they are drawn, and a click on the legend
survives a redraw), latency (p50 and p95) and errors — share one x cursor, and the
**breakdown tables** by model, by environment and by release carry proportion
bars and a **Tokens** column of input plus output, what a bill is made of;
the cell's tooltip lists every class the group reported, reasoning and cache
write among them. Cache read is on the chart, where it explains a bill that is
smaller than the tokens suggest, and out of the column, where it would count
the same tokens twice for the providers that report cached tokens inside the
input. The
bucket switcher is hourly/daily and defaults to hours for windows up to 48
hours, days above. In the release table, the traces that named none are one
row called *(no release)* rather than a row that is missing.

A bucket the server did not return is drawn as a **gap**, never as a zero,
and a bucket that reported no cost has no cost point — nor a token point when
none of its calls reported usage, and a table row without one shows `—`: the
API refuses to fabricate rows and so does the screen. An empty window says so
rather than drawing an empty frame. A project with traces but none in the
window shows dashes, empty axes and the true last-trace time: not the
instructions.

The numbers behind the charts come from an hourly rollup for closed hours and
from the live rows for the hour in progress, which is why a month reads as
fast as a day and why the charts keep answering about data retention has
since deleted. The latency percentiles are histogram-based, accurate to a few
percent — see [api.md](api.md#where-the-numbers-come-from).

**Quality** — up to six score names of the window, the ones with the most
scores first, each drawn as the [Quality](#quality) overview draws its card
and leading to that score's detail, with *See all* when more names exist.
The block is not drawn when the window holds no score. One request, and none
while the block is hidden.

**A fresh project** — no traces at all, which is *Last trace* saying so —
shows, in place of the blocks, the exporter settings with a copy button and
the quickstart link. The filter bar stays.

**Customize** in the header enters a mode in which every block wears a handle
and a *Hide* control. Blocks are reordered by dragging the handle — or by
keyboard: focus the handle, space to lift, arrows to move, space to drop, with
the moves announced — and the order is one list across the summary, the charts,
the breakdowns and the quality cards, so a chart may sit above the summary if
that is what you want. Hidden blocks are listed in a strip at the bottom with
*Show*; *Reset* restores the default; *Done* leaves the mode. A hidden block
makes no request of its own: the timeline request serves the five charts and
is made while any of them shows, and the breakdowns and the quality cards are
each their own request. A person who asked for less motion gets no animation
while dragging; the keyboard path is unchanged.

The arrangement is kept on the **account**, per project, under
`preferences.dashboard` ([accounts.md](accounts.md#preferences)), so it
follows you between browsers and is yours whatever your role in the project.
Every change in Customize mode is written at once; a write the server refuses
is shown in the strip, and the arrangement on screen stays what you made.
Two tabs writing at once is last write wins.

Neither the dashboard nor the Quality screen has a live mode. Both re-read on
a filter change and on the **Refresh** control.

**Users** — sits between Sessions and Prompts, joining the screens about
who: a user is a set of sessions, and their page is the dashboard's charts
for one of them.

The listing is `GET /api/v1/users`: id, traces, sessions, errors, cost,
tokens, first and last seen. A sort select offers the questions the endpoint answers —
last seen, traces, cost, tokens (input plus output, a user with none last),
errors, always descending — and a box narrows by a
**case-sensitive prefix** of the id; both live in the URL. A long id is cut in
the middle, with the whole of it in the title and a copy button beside it.

It is built from an hourly roll-up, so it trails live traffic by a few minutes
and the empty state says so rather than claiming there are no users: a user
first seen just now is on the Traces screen, filtered by their id, and not yet
here. See [users.md](users.md).

**A user** (`/users/{id}`) is composed of parts the other screens already
have: the summary cards from `GET /api/v1/users/{id}` — traces, sessions,
errors, cost, p50/p95, first and last seen, exact including the traffic too
recent for the listing — then a time window governing an **activity** chart
(traces, sessions started and failing traces) and a **cost** chart, both over
`GET /api/v1/stats?user_id=`, then the environment and model breakdowns with
the same filter and the same **Tokens** column as the dashboard's. Under them, two tabs (`?tab=sessions|traces`) hold the
Sessions and Traces tables filtered by this user, each with a ⤢ to the full
listing with the filter set — so the peek panel and its `j`/`k` walk come
along rather than being reimplemented.

*Erase data* in the header is the Settings card with the id already filled in:
the server's own dry run, the id typed back to confirm, and the listing
afterwards ([admin.md](admin.md#erasing-a-users-data)).

The user id in the Traces table and in a trace's header is a link here. It is
the second link in a trace row — the row's own link, the one `j`/`k` and Enter
use, stays first.

**Settings** — see [Settings and administration](#settings-and-administration).

**Trace** — the observation tree on the left, the selected observation on the
right. A node shows its kind, name, duration, cost and whether it failed;
a parent whose descendant failed is marked too, so collapsing a subtree never
hides a failure. Arrow keys walk the tree — up and down move, right opens,
left closes and then leaves.

The kind is an icon — a wrench for a tool call, a shield for a guardrail, a
box for a plain span — each carrying its name for a tooltip and for a screen
reader. There are ten kinds, which is more than a three-letter label can keep
readable.

The detail panel shows the observation's timings (including TTFT, when the
client reported when its first token came back — negative if its clocks
disagreed, shown as sent rather than hidden), level, model, usage and cost,
then `input`, `output` and `metadata` as documents with each payload's size
beside its heading (see [Payloads](#payloads)). Usage,
cost and model parameters are short flat records, and read as two-column lists
rather than as JSON. A generation that ran a named prompt carries a badge
saying which — `support-answer · v7` — that leads to the traces which ran it.
The badge is a link to a filtered listing, not a lookup: this store need not
manage that prompt for the link to work. This is the same view the peek panel
shows — one component, two frames around it.

The header above the trace names its release when it has one, beside the
timestamp, latency and cost.

## Payloads

Every payload — an observation's `input`, `output` and `metadata`, and a
trace's own metadata — is shown in one editor surface, the same one the eval
screens write item bodies in. It is a text document, not a tree:

- **Long strings are whole and wrapped.** A prompt is the thing you came to
  read; nothing is cut at a character count and nothing scrolls sideways.
- **⌘F / Ctrl-F searches inside the payload**, while it has focus. A large
  document is drawn a screenful at a time, so the browser's own find would
  miss what is scrolled away; this one searches the document itself.
- **The gutter folds.** A document over 400 lines opens with everything more
  than two levels deep folded, which is what makes a megabyte navigable; a
  short one opens flat. Searching for text inside a fold opens it. Folding
  happens once the document is on screen rather than before, so the payload
  is there to read either way; a document too large to fold in the time
  allowed opens with its head folded and the rest flat.
- **Copy** takes the whole document, not the part on screen. A preview under
  a truncation banner has no Copy at all: what is on screen there is a
  prefix, and the banner is how you get the rest.
- A payload that is not JSON at all — a plain-text prompt on a span that sent
  one — is shown as the text it is, with no quotes and no escapes.

Light and dark share one set of colours with the rest of the interface: keys,
strings, numbers and punctuation, all of them from `app.css`.

**Images and files** a payload references ([media.md](media.md)) are drawn
above its JSON: an image as a thumbnail with its type and size under it, which
opens the full picture in a new tab; any other file as a chip that downloads
it; and a reference the project's setting kept no bytes for as a muted chip
reading *not stored (project setting)*. The JSON still shows every reference
as data. The bytes are fetched with the page's own credentials and drawn from
memory — never opened as a page of this server — so a file that declared
itself HTML is still only a download. A picture is drawn under the type its
own reference declared, and a download is saved with an extension only for
common image, audio, video, PDF and text types; any other type is saved with
none.

### The response budget

A trace is fetched with `?expand=io`, so the server spends its byte budget on
the payloads and replaces the ones that do not fit with truncation markers
(see [api.md](api.md#the-response-budget)). The interface consumes those markers
rather than working around them:

- a truncated payload shows the preview the marker carried, under a banner
  reading **showing 589 B of 3.8 KB · Load the whole payload**. The banner is
  the button: it fetches `GET /api/v1/observations/{id}/io` — the one endpoint
  no budget applies to — and swaps the whole document in. The preview is a
  prefix cut on a byte boundary, so it is shown as text rather than parsed;
- a marker the budget left no room for a preview in is the banner alone;
- a trace with more payloads than the budget can carry markers for gets none
  of them, and each payload offers a load button of its own.

Nothing is fetched unasked: a wide trace would pull megabytes on open, and
which payload is worth that is the reader's call. Raising
`TRACEPAD_RESPONSE_BUDGET_BYTES` inlines more of them up front.

## Settings and administration

Settings is **three tabs, because it is three audiences**. The active one is
in the address, so a tab is a link. Two of the tabs are about a project and
live under its prefix (`/p/{project}/settings/project`, `…/settings/server`);
the Server tab is about the whole server and sits there anyway, and switching
projects from either keeps the tab. The Account tab is about you and lives
bare, at `/settings/account`: from there Project and Server point at the
project you last looked at, and are absent when you reach none.

**Project** — the project on screen, for everybody who can reach it:

- **Project** — its name and id. Renaming is an owner's, because a project's
  name is the echo every destructive confirmation is typed against; the field
  says so to everybody else rather than simply not being there.
- **Retention** — all three windows, with `null` spelled out: "keep forever"
  for the queryable data, "follow the window above" for the raw bodies, and
  "keep forever" again for the statistics history, which outlives the traces
  it summarizes and so has a window of its own (see
  [retention.md](retention.md#what-outlives-what)) — and beside them the
  project's media setting: store images and files once each, or keep a
  placeholder only ([media.md](media.md#not-keeping-them-the-placeholder-setting)).
- **API keys** — each key's name, public key and [scopes](api.md#scopes),
  when and by whom it was minted, and when it was last used ("never" until it
  is); a form to mint one — a name and a checkbox per scope, `ingest` ticked
  by default, since wiring an application is the usual reason to mint — and
  revocation. A key whose minter can no longer manage keys
  in the project — removed, demoted to viewer, disabled or deleted — says so on
  its row, because taking their access away revoked nothing
  ([admin.md](admin.md#keys)). A minted pair is shown **once**, with the lines
  that fit its scopes: `TRACEPAD_API_KEY` for every key, and the OpenTelemetry
  and Langfuse variables only for a key with `ingest`, so a key that cannot
  send is never offered to an exporter. The secret is stored as a hash and the
  dialog says so rather than implying it can be found again.
  Deleting an account on the Server tab lists the keys it minted before it
  asks for the email.
- **Danger zone** — erasing everything stored about one end user.

A `viewer` sees the cards and one line saying why the buttons are gone —
what the project is set to is worth knowing even when changing it is not
yours to do. Retention and the keys are an `editor`'s.

**Account** — everybody's own: the display name, the password (changing it
signs out every other browser), and the list of browsers you are signed in
on, with *Sign out everywhere else*.

**Server** — an owner's, and absent for everybody else; the address
redirects too. Two tables:

- **Projects** — every project on this server with its retention and its
  status, and the way into each one: a row's name and its *Settings* lead to
  that project's Project tab, which is where the name, the retention and the
  keys are changed — nothing is edited from the table itself. *Delete* sits
  beside *Settings* behind the echo; a soft-deleted row shows its purge date
  and *Restore* instead. *New project* opens the keys-once dialog (the same
  one the sidebar's switcher opens), and a project created here is opened
  once its keys are put away — on this same tab, under the new project,
  where the table is the same one. On the Project tab an owner has
  *All projects* above the cards, the way back to this table.
- **Accounts** — email, name, standing (`pending` / `active` / `disabled`),
  last login, and the projects each account reaches with the role it has.
  *Invite* opens a dialog and hands back the link once, with a copy button
  and its expiry. *Edit* changes the same fields, disables an account, or
  mints a fresh link. *Delete* is the dry run with the email typed back.

`TRACEPAD_ADMIN_TOKEN` is not entered anywhere in the interface any more: an
owner reaches all of this, and a credential the screens do not use is a
credential the screens should not hold.

Every destructive action is the server's dry-run/confirm contract rendered
(see [admin.md](admin.md#dry-run-by-default)): the card asks the API
what the change would delete, shows that answer, and enables its button only
once you have typed back the identity the server named — the project's name,
or the user id; [deleting one trace](#deleting-traces) is the one card that
fills its echo in, because the id is already on screen. Nothing is counted in
the browser, and a refusal is reported in the server's own words.

## State in the URL

Filters, the search, live mode, the time window, the dashboard's bucket and
the selected observation all live in the query string, and the project in the
path (see [Opening it](#the-project-in-the-address)), so any view is a link:
`/p/{project}/dashboard?from=…&to=…&group_by=hour`,
`/p/{project}/traces?q=refund+failed`,
`/p/{project}/traces?status=error&environment=prod`,
`/p/{project}/traces/{id}?obs={observation_id}`,
`/p/{project}/sessions?environment=prod`,
`/p/{project}/traces?environment=production,staging`,
`/p/{project}/users?sort=cost&prefix=acme:`,
`/p/{project}/users/{id}?tab=traces&from=…`.
Reloading, sharing and the back button all behave, and so does the same link
with the prefix left off — it lands on the remembered project.

The time window is one control on every screen that has one — presets for the
last hour, day, week and month, plus a calendar — and it travels as the
`from`/`to` the API itself takes. A preset sets `from` and leaves the end
open, so "the last 24 hours" keeps ending now.

### The remembered window

The window you last set is **remembered in this browser**, per account: a
preset as the preset, so "the last 24 hours" still ends now a week later; a
calendar range as its days. The screens that open on a default window — the
dashboard, Quality and a user's page — open on the remembered one instead
when their address carries none; every screen with the control writes it,
the Traces and Sessions listings included. The listings never *read* it: they
keep opening on *Any time*, because a listing that silently narrowed itself
to last week would hide the trace you came for. A window in the address
always wins, so a link somebody sends shows their window, not yours. Nothing
about it goes to the server — the arrangement of the dashboard does, the
window does not — and *Any time* forgets it.

## Appearance

Light and dark ship together and follow the operating system by default; the
toggle in the sidebar pins one and remembers it.

Everything the page needs is inside the binary — fonts included. The
interface makes **no request to any external origin**, which an air-gapped
install depends on and which the end-to-end suite asserts.

The layout is usable on a phone. The sidebar becomes a slim bar on top —
the product, the project and the account — and a tab bar at the bottom with
*Dashboard*, *Traces*, *Sessions*, *Users* and *More*; *More* opens a sheet
with every other section, and lights up while one of them is on screen. The
sheet closes on Escape, a tap outside it or a swipe down from its grip. The
filters live in a popover, tables scroll inside their own box rather than
scrolling the page, and the trace screen switches between the tree and the
observation instead of showing both. The project's listings — traces, sessions, users,
prompts, datasets and their items, runs and their items, queues and their
items, score configs, the Accounts, Projects and API keys cards, the
Stats screen's and a user's breakdowns, a run's scores and a comparison's scores and cases — do not
scroll at all there (the Quality screen's breakdowns do not fold: at 240 px they fit
any box, and one with a longer label scrolls in its own box):
each keeps the columns that say which row it is and whether it went wrong —
for a trace, the time, the name and the errors — and folds the others into
lines under the row, which wrap between values rather than cut any of them,
so the environment, the latency, the TTFT and the cost read
`production · 2.08 s · TTFT 180 ms · $0.0054` under the name, with the user
and the session, still links, beneath, and a session's traces read
`14 traces · $0.1742`. A breakdown keeps its key, its count and its errors
with their bars, and puts the cost and the tokens under the key; a compared
case lists its scores one per line under *Scores*. A queue's *Reopen* and
*Remove* become icons there, and an account's *Edit* and *Delete* and a
project's *Settings* and *Delete* stack beside it. A listing folds on a desktop
too wherever its box is narrower than the whole table — a narrow window or a
tablet beside the sidebar, or a session's traces inside the peek panel —
because the width it answers to is its box's, not the screen's, and in rem, so a larger default font size folds it earlier. A long id or name that a program gave is cut to its column, with the whole of it on hover, rather than widening the table past the width it folds at; what a person typed (an API key's or a project's name) wraps, and in a table's whole layout an ordinary value (a timestamp, a key, an id) is not torn across lines: where the table would need more room for that, it folds earlier. A screen's own header keeps its 48 px as
a floor rather than a height: at a phone's width, what it carries beside the
title — a breadcrumb, an identifier, a count — takes a second line under it,
and the screen's controls stay on the first one. Wider than that it is the
single row it has always been, and a name too long for the line is shortened
to an ellipsis rather than moved.

## What the page may load

The interface renders text it did not write — prompts, completions, tool
arguments, whatever an application put in a span — so the page it is served in
carries a `Content-Security-Policy` that limits what can run in it even if some
of that text ever reached the page as markup. The server sends it with the
page and with no other response:

```
default-src 'none'; script-src 'self' 'sha256-…'; style-src 'self' 'unsafe-inline';
img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; worker-src 'none';
base-uri 'none'; form-action 'self'; frame-ancestors 'none'
```

- **Script** is the bundle's own files and the one inline script that starts
  them, named by its hash. The server takes the hash from the page it is about
  to send when it starts, so a rebuilt interface needs nothing configured.
  There is no `'unsafe-inline'` and no `'unsafe-eval'`: an injected `<script>`,
  an `onerror="…"` or a string passed to `new Function` is refused.
- **Requests** go to the server the page came from and nowhere else
  (`connect-src 'self'`, and `form-action 'self'` for a form). A script that
  somehow ran could not post a key or a trace to another origin.
- **Pictures** are the server's own, `blob:` for the ones the interface draws
  from bytes it fetched with your session ([media](media.md)), and `data:` for
  the page's empty icon. **Fonts** are two files in the bundle.
- **Style** is the exception: it may be inline. The editor writes its
  stylesheet into a `<style>` element and the dialogs set the page's `style`
  attribute, and neither can carry a hash. It is harmless to the page's data,
  because a style cannot fetch from anywhere the other lines do not allow.
- Everything else — frames, objects, workers, a `<base>` — is refused.

A refusal is a line in the browser's console. If a change to the interface
trips one, the fix is in the change; the policy is not widened without a
decision in [spec 051](../specs/051-content-security-policy.md). `npm run dev`
is Vite's server and sends no policy, so the production build is where to look.
A reverse proxy that adds a `Content-Security-Policy` of its own adds a second
policy, and the browser enforces both: it can narrow this one, never loosen it.

## Builds without it

The interface is compiled by Node and Vite, which `go build` cannot run. A
plain source build therefore ships without it and serves a page that says so;
the API, the CLI and the MCP server of such a build are complete.

Every official artifact carries it — the release binaries and the
[Docker image](docker.md), whose own build runs Node before Go for exactly this
reason, and the Homebrew tap when there is one — as does a local `make build`,
which builds the bundle first. `go install` is not a supported channel for this
reason.

## Working on it

The sources are in `ui/`: a SvelteKit SPA (`adapter-static`, no SSR) with
Tailwind v4, built by Vite into `ui/dist`. The only runtime dependencies are
bits-ui (headless primitives), Lucide (icons), uPlot (the four charts) and
CodeMirror 6 (the payload surface, imported statically and pinned by exact
version; there is no `basicSetup` — the extensions are listed by hand in
`ui/src/lib/components/json/setup.ts`).

It builds with Node 24.15 or a newer 24.x, the one Node `ui/package.json` names in `engines`:
the targets below stop at once, naming the version they found, under any
other. The type check is `svelte-check` on TypeScript 6.0: it imports the
compiler as a library, which TypeScript 7 has no stable API for yet, so the
`typescript` package is an alias to 6.0's. The interface moves to 7 when 7.1
ships that API and `svelte-check` runs on it.

```sh
make ui          # build the bundle and stage it for embedding
make build       # the above, then `go build -tags ui`
make ui-check    # svelte-check, unit tests, and the API-type drift check
make e2e         # boot the real binary on a temp database, run Playwright
```

`npm run dev` inside `ui/` serves the app with hot reload and proxies `/api`
to a `tracepad serve` running on the default port.

The TypeScript types of the API are generated from
`internal/server/openapi.json` and committed; `make ui-types` regenerates
them, and the gate fails if the committed copy has drifted.
