# Spec 008 — The Peek Panel: a row opens beside the listing it came from

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Reading a listing is a loop: open a row, judge it, come back, open the next
> one. Specs 006 and 007 made every row a navigation, which throws the listing
> away and rebuilds it — filters, cursor pages, live mode and scroll position
> included — on the way back. This spec keeps the listing on screen and opens
> the row beside it, in a panel that slides in from the right, without taking
> away the full-page routes that make a view a link.

---

## Overview

Deliverables:

- A **peek panel**: a right-hand slide-in panel over the current screen, with
  the listing still live underneath it, driven by `?peek=` in the URL.
- **Traces** and **Sessions** listings, and the trace table inside a session,
  open their rows in it.
- A session peeks with **one level of drill-down**: a trace inside a session
  panel replaces the panel's body and a breadcrumb leads back.
- The detail bodies (`TraceDetail`, `SessionDetail`) extracted from their
  routes so that the page and the panel render the same component.
- The full-page routes `/traces/{id}` and `/sessions/{id}` are unchanged and
  stay the canonical, shareable form; the panel links to them.

Nothing on the server changes: this is a second arrangement of the same two
GETs the screens already make.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-08-29** — A row opens in a **peek panel over its listing**, not by navigating to the detail route. The full-page routes stay exactly as they are, and the panel carries a control that opens the row there | The listings are read in a scanning loop, and specs 006/007 priced every step of it at a full navigation: coming back re-runs `GET /api/v1/traces` from the first cursor page, discards every "load more" already paid for, restarts live mode and loses the scroll position — for a decision that usually takes two seconds of looking at the tree. Langfuse is the precedent our audience already has the muscle memory for. Keeping the full page rather than replacing it is what makes this cheap: the panel is a second view of a component that already exists, and the URL a person sends a colleague is still a page and not an app state |
| 2 | **2026-08-29** — What the panel shows lives in the **URL of the listing**, as `?peek={id}` beside the filters, and the observation selection keeps the `?obs=` of spec 006. A session panel drilled into a trace carries `&trace={id}` as well | Spec 006's contract is that any view a person is looking at is a link they can send, and the panel is a view. It also settles the reload question for free — a reopened tab is looking at what it was looking at — and it makes the Back button close the panel, which is the gesture every reader tries first |
| 3 | **2026-08-29** — The row keeps its real `href` to the full page; only an **unmodified left click** is intercepted to open the panel. ⌘/Ctrl/Shift/middle click still opens the detail route, in a new tab where the browser says so | A listing whose rows are `<div onclick>` cannot be opened in a background tab, cannot be copied as a link, and is invisible to anything that reads links. The interception is the enhancement; the link underneath is what the row actually is |
| 4 | **2026-08-29** — On a wide screen the panel is **not modal**: no scrim, no focus trap, no scroll lock. The listing underneath stays scrollable and clickable, and clicking another row swaps the panel's contents in place. It is `role="dialog"` with `aria-modal="false"`, labelled by its own heading; Escape closes it and returns focus to the row that opened it | The whole point of Decision 1 is that the listing survives; a scrim that dims it and a trap that fights every click on it would take back what the panel was built to give. Clicking down a column of rows and watching the panel follow is the fastest way anybody reads a listing, and it is only possible if the panel does not claim the page. Non-modal is also the honest ARIA: `aria-modal="true"` would tell a screen reader the rest of the page is inert while it demonstrably is not |
| 5 | **2026-08-29** — Width is `clamp(34rem, 100vw - 30rem, 60rem)`, and the panel sits beside the listing only from the **`lg`** breakpoint up; below it, it covers the viewport | Sixty rems is what the trace detail's two panes want before the tree starts wrapping names, but a *share* of the viewport is the wrong measure: a proportional 72 % left 150 px of listing on a 1280 px laptop — narrower than the filter bar, which the panel then covered and made unclickable (found by the e2e test that tries to type in it). Subtracting a constant instead reserves what the listing actually needs — the sidebar plus its first columns — and gives the panel everything above that, up to the cap. The floor of 34 rem is where the two panes stop fitting at all, which is why the cutover is `lg` and not `md`: between 768 and 1024 there is not enough room for both, so one of them has to be the whole screen |
| 6 | **2026-08-29** — History: going **deeper pushes** one entry (opening a panel, drilling into a trace), moving **sideways or up replaces** it (another row, the breadcrumb, closing) | Back closes the panel, which is Decision 2's reason for existing, and Back leaves a drilled-in trace for the session it came from. Pushing per row instead would bury the listing under twenty entries and make Back a slow rewind through a scan the reader has already finished; replacing keeps "one gesture back to where I was" true no matter how long the scan was |
| 7 | **2026-08-29** — The panel's previous/next controls walk the **rows the listing has already loaded**, in the order they are on screen, and are disabled at either end. They are not bound to the arrow keys | The listing is keyset-paginated and possibly live: "next" can only mean the next row on screen, and a control that silently fetched the next cursor page would be a second, invisible pagination with its own failure mode. The arrow keys stay unbound because the observation tree inside the panel already owns them (spec 006, accessibility floor), and a panel-level binding would steal them from the thing being read |
| 8 | **2026-08-29** — The route bodies become `TraceDetail` and `SessionDetail` components. Each loads what it shows and hands it back through a bindable prop, so the caller can name it in chrome of its own: the page wears the shell's `PageHeader`, the panel wears a breadcrumb, an expand control and a close button | Two copies of the trace detail would drift within one sprint — the tree, the payload budget flow and the phone pane switch are the most intricate code in the interface, and none of it is chrome. The binding rather than a `header` snippet because the chrome does not live in the same box in both cases: the page's header is the shell's, above the scroll container, and the panel's is the panel's. Handing the loaded object outward keeps one fetch, one owner, and two frames |
| 9 | **2026-08-29** — A session panel drills **one level**: its trace table opens the trace *inside the same panel*, with a breadcrumb back to the session. It does not stack further, and the trace layer has no previous/next controls | A session is a conversation, and reading it means walking its traces without losing the session — that is the whole reason the sessions listing has a panel at all. One level is where the cost stops: a general stack needs its own back-stack, an answer to what Escape closes at depth three, and a URL grammar for the path. The trace layer has no previous/next because the neighbours there belong to the session's own table, which the layer above owns and this layer has replaced; the breadcrumb is one click away from it |
| 10 | **2026-08-29** — A trace row inside a **session page** (`/sessions/{id}`) opens a trace panel too, and the row highlight marks which one is open on every listing that has a panel | The session page is a listing like any other, and the reason of Decision 1 does not weaken because the listing is nested. The highlight is not decoration: with a non-modal panel and no scrim, the row is the only thing that says where the panel's contents came from |
| 11 | **2026-08-29** — The slide-in is 160 ms, cancelled entirely under `prefers-reduced-motion` | Spec 006 #6 allows motion on state changes only, and a panel arriving from off-screen is the state change that most needs it: without it, half the screen is replaced between two frames and the eye has to re-find the listing. One hundred and sixty milliseconds is inside that spec's 100–200 ms band, and reduced motion turns it into an instant swap rather than a slower one |
| 12 | **2026-08-29** — **A phone gets the panel too**, not the old navigation: below `lg` it covers the viewport and, at those widths only, becomes modal — `aria-modal="true"` and Tab held inside it. There is no second interaction model for narrow screens | The panel's visible benefit (a listing beside the detail) is worth nothing at 375 px, but its mechanical one is worth most there: closing it is instant and free, where a navigation back to the listing re-runs `GET /api/v1/traces` from the first cursor page, drops every "load more" already paid for and loses the scroll position — on the slowest connection the product has. Keeping the old navigation on phones instead would fork the interaction model at exactly the width that gets tested least, and Decision 8's shared body means there is nothing to fork *into*: the same component renders either way, and only the container's classes differ. Modality follows the geometry rather than the pointer (a departure from spec 006 #15's rule, on purpose): the reason to trap focus is that the listing is *covered*, which is a fact about width, not about fingers |
| 13 | **2026-08-29** — The previous/next controls carry the shortcuts **`k`** and **`j`**, drawn on the buttons as keycaps and dimmed along with the button when the listing has run out. They are ignored while a letter is being typed and while any modifier is held | Amends Decision 7, which ruled out only the *arrow* keys, and for a reason that does not reach here: the observation tree owns the arrows, while `j`/`k` are free — which is exactly why the tools this audience already uses settled on them. Drawing the key on the control is what makes a shortcut discoverable without a cheat sheet, and it costs nothing: the button is already there and already says what it does. Dimming the keycap with the button is the same message twice for two different readers — the one looking for a control to click, and the one who just pressed the key and needs to know why nothing moved. The guard is not optional: without it, typing `prod` into the environment filter would walk the listing four rows |
| 14 | **2026-08-29** — The UI-line budget in `scripts/ui-lines.sh` moves from 7,300 to 8,100 | Same reasoning as spec 007 #13: the number is the budget of the newest spec that moved it, so that the warning fires when *this* spec is exceeded rather than at one two specs old. This spec lands at 8,016 — a panel, two extracted bodies, the URL helpers and their tests — and 8,100 leaves the margin a small follow-up needs without hiding the next screen-sized addition. Design §8's 6–9k envelope for the finished interface is unchanged, and this is now the second half of it: the room left is one screen, not four, which is a fact worth having the warning say out loud |
| 15 | **2026-08-29** — The row's click handler sits on the `<tr>`; the link stays in the first cell at its own size, rather than being stretched over the row with an `::after` overlay. A click that ended a text selection, and the second click of a double-click, open nothing | The stretched link is the usual trick for "the whole row is clickable", and it quietly costs the thing a listing is read for: the overlay covers every cell, so dragging across a trace id starts a link drag instead of a selection, and the id can only be retyped into the terminal. Reported from use, and true of both listings since spec 006. Moving the handler to the row buys the same hit area without covering anything. The selection guard is what lets both be true at once — without it, a drag that ends inside the row also opens a panel nobody asked for — and the double-click case is the same argument for the gesture that selects a word. The link is still there, unstretched: it remains the keyboard's path to the trace and what ⌘-click opens (Decision 3) |
| 16 | **2026-08-29** (from the PR #10 review) — **Enter on a row opens the panel**, exactly as a plain click does. The full page is what ⌘-click, a middle click and anything reading the link get, and what ⤢ leads to from inside the panel | Amends the last sentence of Decision 15, which claimed the link was the keyboard's path to the *page* while the code — rightly — intercepted keyboard activation too. Of the two ways to make that sentence true, honouring the link is the wrong one: a control that does one thing under the mouse and another under the keyboard has to be learned twice, and it would leave keyboard users the only people who cannot reach the panel from a listing at all. The page is not lost, it is one Tab away on ⤢ |

## Application contract

Routes are unchanged. The panel is query state on the listings:

- `/traces?peek={trace_id}` — the trace panel; `&obs={observation_id}`
  selects a node inside it, exactly as on `/traces/{id}`.
- `/sessions?peek={session_id}` — the session panel;
  `&trace={trace_id}` replaces its body with that trace (`&obs=` applies to
  it), and dropping `trace` returns to the session.
- `/sessions/{id}?peek={trace_id}` — a trace panel over a session's table.
- `/traces/{id}` and `/sessions/{id}` are what they were: the full page, the
  link the panel's ⤢ control leads to, and what an unmodified row click would
  have reached before this spec.

A `peek` naming a row that does not exist renders the panel's own failure
state — the same error line the full page shows — rather than a blank panel
or a silent redirect.

**The panel.** Fixed to the right edge, full height, one border on the left
and the system's single elevation (spec 006 #6). Its header carries, from the
left: the breadcrumb (session panels drilled into a trace) or the title, the
same meta the full page shows (timestamp, latency, cost, id with its copy
button), then previous ⌃`k` / next ⌄`j`, ⤢ (opens the full page) and ✕. Below
it, the detail body, which scrolls inside the panel. The two walking controls
wear their key as a keycap, which dims with them at the ends of the listing;
on a coarse pointer the keycap is not drawn, because there is no keyboard to
draw it for.

**Keyboard.** `k` and `j` move to the previous and the next row (Decision 13);
they do nothing at the ends, nothing while a text field has focus, and nothing
with a modifier held. Escape closes the panel and returns focus to the row that
opened it; in a drilled-in session panel it closes the whole panel, and the
breadcrumb is how the session comes back (Decision 9). Everything inside the
body keeps the keyboard behaviour it has on the full page, arrow keys
included.

**Live mode.** A trace panel open over a live listing stays open across
polls; the merge (spec 006 #12) can move the row it came from, and the
highlight moves with it because both are keyed by id.

## Testing

- **Component (vitest)**: the URL round-trip of `peek`/`trace`/`obs` — open,
  switch rows, drill, come back, close — asserted on the search string the
  helpers produce; neighbour selection at both ends of a loaded listing;
  a modified click is not intercepted, and neither is a click that came out
  of a text selection.
- **E2E (Playwright)**: a row on `/traces` opens the panel with the listing
  still on screen and the row lit; ⤢ lands on `/traces/{id}` showing the same
  observation; Escape closes and the URL loses `peek`; a session row opens
  the session panel, a trace inside it drills in, the breadcrumb returns, and
  a reload of the drilled-in URL comes back to the same place. `j` and `k`
  walk the rows and stop at the ends, and neither does anything when typed
  into a filter field. A drag across a cell selects its text and opens
  nothing. At 375 px the panel covers the viewport.
- **Accessibility floor**: the panel is labelled and focus moves into it on
  open and back to the row on close; previous/next are disabled, not hidden,
  at the ends and name their shortcut with `aria-keyshortcuts`; every
  icon-only control in the header is named (spec 006 #14).

## Out of scope

- A general panel stack (more than the one level of Decision 9).
- Panel width the reader can drag, and remembering it.
- previous/next crossing a cursor page boundary by fetching the next one.
- Any change to the read API: this spec adds no request the screens did not
  already make.
