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

- **First run.** The server prints a pre-authed link next to the connection
  strings (`http://localhost:4318/#key=tp-sk-…`). Open it and the interface
  is signed in. The key travels in the URL fragment, which browsers never
  send to a server; the app stores it and removes it from the address bar.
- **Later runs.** The server prints the plain URL, and the login screen asks
  for a project key.
- The key is kept in `localStorage` and sent as `Authorization: Bearer`. A
  `401` clears it and returns to the login screen. "Sign out" in the sidebar
  forgets it.

The interface reads traces, which needs a **project key**. The admin token is
a control-plane credential (see [admin.md](admin.md)) and the login screen
refuses it with an explanation. It is entered further in, on the Settings
screen, where the endpoints it can actually call live.

## Screens

**Traces** — the listing. One row per trace, mapping onto
`GET /api/v1/traces`: time, name, environment, user, session, cost, latency,
TTFT and how many observations failed. TTFT sits beside latency because the
two answer the same question from opposite ends — how long the whole run took,
and how long somebody waited before anything appeared. The filter bar offers
exactly the filters the endpoint accepts (`q`, `from`, `to`, `environment`,
`user_id`, `session_id`, `name`, `tag`, `status`, `min_cost`, `release`,
`version`, `type`, `prompt`) — a test reads `openapi.json` and fails if the
two ever disagree. The bar underneath turns the pages.

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

**Sessions** — one row per session over `GET /api/v1/sessions`: last seen,
id, how many traces, how many of them failed, cost, first seen. The filters
are the endpoint's four (`from`, `to`, `environment`, `user_id`), and a row
opens the session: its totals over its traces, and a trace opens from there.
Every number counts traces, which is what a session is a collection of.

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
spec 003's correction, which replaces the row rather than adding a second one.
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

*Prompts* is a top-level section, between *Stats* and *Evals*: a prompt is a
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
the `prompt=` filter [spec 012 added](api.md#filters). A `?version=` the name
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
prompt, one per message for a chat one, each with a role beside it (`system`,
`user`, `assistant` offered, anything accepted) and *add*, *remove*, *up*,
*down*. `config` is the same JSON surface every payload uses. *Save* posts one
version and lands on it. The gate is the server's own rules mirrored at the
fields — the name's grammar, a non-empty body, a role and content per message,
a config that parses, and `latest` refused as a label — so that a `400` naming
one of them is not how you find out; the server still decides.

**Delete** takes the name whole: every version and every label, in one
transaction. It wears the ceremony every act with a blast radius wears here —
the server's dry run on screen, with what it would take and what it would not,
and the name typed back. Traces that ran the prompt keep the name and version
they recorded, so the trace filter goes on answering for it.

## Evals

The sidebar's *Evals* section is the eval nouns of [datasets.md](datasets.md)
on screen: **Datasets**, **Runs** and **Score configs**. Every table is the
shared listing (pages in the URL, the bar underneath), every row opens in the
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

**Stats** — four charts over `GET /api/v1/stats` — traces, cost, latency
(p50 and p95) and errors — sharing one x cursor, plus breakdown tables by
model, by environment and by release with proportion bars. The bucket switcher
is hourly/daily and defaults to hours for windows up to 48 hours, days above.
In the release table, the traces that named none are one row called
*(no release)* rather than a row that is missing.

A bucket the server did not return is drawn as a **gap**, never as a zero,
and a bucket that reported no cost has no cost point: the API refuses to
fabricate rows and so does the screen. An empty window says so rather than
drawing an empty frame.

The numbers behind the charts come from an hourly rollup for closed hours and
from the live rows for the hour in progress, which is why a month reads as
fast as a day and why the charts keep answering about data retention has
since deleted. The latency percentiles are histogram-based, accurate to a few
percent — see [api.md](api.md#where-the-numbers-come-from).

Neither screen has a live mode. Both re-read on a filter change and on the
**Refresh** control; the Traces live toggle is the only poller in the app.

**Users** — sits between Sessions and Stats, which is what it joins: a user is
a set of sessions, and their page is Stats for one of them.

The listing is `GET /api/v1/users`: id, traces, sessions, errors, cost, first
and last seen. A sort select offers the four questions the endpoint answers —
last seen, traces, cost, errors, always descending — and a box narrows by a
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
the same filter. Under them, two tabs (`?tab=sessions|traces`) hold the
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

Settings runs on **two credentials with disjoint powers**.

The session's project key manages its own project, which is what a project
key is for (see [admin.md](admin.md)):

- **Project** — its name and id. Renaming needs the admin token, because a
  project's name is the echo every destructive confirmation is typed against;
  the field says so and names `tracepad projects rename`.
- **Retention** — all three windows, with `null` spelled out: "keep forever"
  for the queryable data, "follow the window above" for the raw bodies, and
  "keep forever" again for the statistics history, which outlives the traces
  it summarizes and so has a window of its own (see
  [retention.md](retention.md#what-outlives-what)).
- **API keys** — the public keys with their dates, minting, and revocation.
  A minted pair is shown **once**, in both connection formats, exactly as
  first run prints them; the secret is stored as a hash and the dialog says
  so rather than implying it can be found again.
- **Danger zone** — erasing everything stored about one end user.

Below them, **Administration** unlocks with this server's
`TRACEPAD_ADMIN_TOKEN` and covers project lifecycle only: list (soft-deleted
projects included, with their purge dates), create, delete, restore. The
token is stored separately from the project key and is sent only to those
endpoints — it never reads a trace. "Lock" forgets it, and so does signing
out.

Every destructive action is the server's dry-run/confirm contract rendered
(see [admin.md](admin.md#dry-run-by-default)): the card asks the API
what the change would delete, shows that answer, and enables its button only
once you have typed back the identity the server named — the project's name,
or the user id. Nothing is counted in the browser, and a refusal is reported
in the server's own words.

## State in the URL

Filters, the search, live mode, the time window, the stats bucket and the
selected observation all live in the query string, so any view is a link:
`/traces?q=refund+failed`, `/traces?status=error&environment=prod`,
`/traces/{id}?obs={observation_id}`,
`/sessions?environment=prod`, `/users?sort=cost&prefix=acme:`,
`/users/{id}?tab=traces&from=…`, `/stats?from=…&to=…&group_by=hour`.
Reloading, sharing and the back button all behave.

The time window is one control on every screen that has one — presets for the
last hour, day, week and month, plus a calendar — and it travels as the
`from`/`to` the API itself takes. A preset sets `from` and leaves the end
open, so "the last 24 hours" keeps ending now.

## Appearance

Light and dark ship together and follow the operating system by default; the
toggle in the sidebar pins one and remembers it.

Everything the page needs is inside the binary — fonts included. The
interface makes **no request to any external origin**, which an air-gapped
install depends on and which the end-to-end suite asserts.

The layout is usable on a phone: the sidebar becomes a two-row top bar, the
filters live in a popover, tables scroll inside their own box rather than
scrolling the page, and the trace screen switches between the tree and the
observation instead of showing both.

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
