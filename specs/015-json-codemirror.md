# Spec 015 — JSON on CodeMirror: one surface for reading and writing payloads

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Every payload the web interface shows — an observation's input, output and
> metadata — goes through a hand-written lazy tree of about a hundred lines
> that folds two levels deep, cuts long strings at 180 characters and knows
> nothing about text: no search, no selection across nodes, no way to see a
> prompt as the prompt it is. The eval screens that follow (spec 016) need
> the same payloads *edited*, and a tree cannot be typed into. This spec
> replaces the tree with one CodeMirror 6 surface that reads and writes
> JSON, keeps every truncation affordance the read API promises, and is
> themed from the same tokens as everything else.

---

## Overview

Deliverables, one PR (REPOS §2):

- A `JsonView` component (read-only) and a `JsonEditor` component
  (editable), both over one CodeMirror 6 setup — `$lib/components/json/`.
  `JsonNode.svelte` is deleted; `Payload.svelte` renders `JsonView`.
- Truncation kept whole: a budgeted payload shows the marker's `preview` as
  text under a banner that names the size and loads the whole payload
  through `/observations/{id}/io` (spec 004 #2, #3, #25).
- A CodeMirror theme built from the app's tokens, light and dark, with the
  two existing code colours and two new ones, all under the contrast test.
- In-document search, folding, line wrapping, copy.
- The UI line budget raised to **14,000** application lines (Decision 9),
  the bundle measured and its ceiling stated.
- `docs/ui.md` updated (payloads section); `smoke.spec.ts`'s truncation test
  rewritten; a `json.spec.ts` of its own.

No server change. No new screen. `JsonEditor` ships with unit tests and no
consumer — spec 016 is its first — which is the deliberate exception to "no
code without a screen": the editor is the same component as the viewer with
one prop flipped, and shipping the viewer without it would mean touching
this component again a week later.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-03** — CodeMirror 6 is the one JSON surface: it **replaces** the lazy tree for reading as well as writing (owner decision 2026-09-03). Packages: `@codemirror/state`, `@codemirror/view`, `@codemirror/language`, `@codemirror/lang-json`, `@codemirror/commands`, `@codemirror/search`, `@codemirror/lint`, `@lezer/highlight`. No `basicSetup` bundle — the extensions are listed by hand | Two ways to show JSON would be two answers to "what does this payload look like", diverging on every detail from string cutting to colours; spec 010 #1's argument for one listing loader applies to one payload surface. CodeMirror over a hand-rolled text view because the pieces that matter — folding on a lezer parse, search over a virtualized document, an editable buffer with diagnostics — are each weeks of work to get right and are the library's whole purpose. `basicSetup` is refused because it pulls in autocompletion, bracket matching, a history and keymaps this surface does not want, and a bundle line the PR cannot explain. |
| 2 | **2026-09-03** — One component, two modes: `JsonView` and `JsonEditor` are thin wrappers over `CodeArea` (`readonly` on or off). A document is a **string**; the viewer is given a JSON value and stringifies it with two-space indentation itself; the editor is given text and gives back text, and reports whether it parses | The mode is one `EditorState.readOnly` facet and one keymap; two components would be the same 200 lines twice. The viewer takes a value because every caller has one (the API's JSON), and the editor takes text because an invalid document must stay on screen while the author fixes it — a value cannot hold a syntax error. |
| 3 | **2026-09-03** — Truncation is the marker's business, not the editor's: `Payload.svelte` keeps detecting `{"truncated": true, …}` (`isTruncated`, `Payload.svelte:8-15`), shows the marker's `preview` **as text** in a read-only `JsonView` with a banner — *showing 4.0 KiB of 212 KiB · Load the whole payload* — and swaps in the whole document when `/observations/{id}/io` answers. The banner is a button with the same accessible name the existing e2e clicks (`Load the whole …`) | Spec 004 #2 puts the choice of spending budget with the consumer, and the click is that choice; the viewer rendering a preview it cannot parse (a prefix cut at a UTF-8 boundary is rarely valid JSON) is exactly why the surface is text and not a tree — the old tree could not show a preview at all and printed it in a `<pre>` beside itself (`Payload.svelte:72-87`). Nothing in the API changes and nothing new is fetched unasked (the alternative, loading whole on mount, was rejected by the owner 2026-09-03 for the reason spec 004 #2 gives: a wide trace would pull megabytes on open). |
| 4 | **2026-09-03** — Long strings are shown **whole**, wrapped (`EditorView.lineWrapping`); the tree's 180-character cut and its per-string expander go away. Documents longer than 400 lines open with every node deeper than level 2 **folded**; shorter ones open flat; the fold gutter is always there | A prompt is the thing the reader came to read, and the tree cut it at 180 characters with a "…" the reader had to click one string at a time. Wrapping keeps the horizontal scroll out and the text in view. Folding by depth on long documents is the tree's "two levels open" rule (`JsonNode.svelte:41`) carried over where it still earns its place — a 5 MB payload after Decision 3's load is a wall — and dropped where it only hid three keys. |
| 5 | **2026-09-03** — In-document **search** (`@codemirror/search`, `Cmd/Ctrl-F` while the payload has focus) is part of the surface, not an extra | CodeMirror renders the visible lines only, so the browser's own find — the one thing the old tree did support, by rendering everything — stops seeing what is scrolled away. A payload viewer where find silently misses text is worse than the tree; the search panel is the price of virtualization and is paid here. |
| 6 | **2026-09-03** — The theme is the app's: a `HighlightStyle` over `@lezer/highlight` tags reading `--color-code-string`, `--color-code-number` (existing, `app.css:61-62`) and two new tokens `--color-code-key` and `--color-code-punct`, plus the surface, border, selection and gutter colours from the existing tokens; every new text token joins `theme.contrast.test.ts`'s `TEXT` list. Light/dark switches through `light-dark()` as everything else does; no CodeMirror theme package | Design §8: `app.css` is the token file and the two themes are one set of tokens. A shipped CodeMirror theme carries its own palette and its own idea of dark, and would be the one part of the screen that ignores the toggle. Two new tokens because keys and punctuation are what a JSON reader distinguishes at a glance; `fg`/`muted` would make a document one colour. The contrast test is the promise that the new colours read on every surface in both themes (`theme.contrast.test.ts:39-51`). |
| 7 | **2026-09-03** — The editor keymap is `defaultKeymap` + `historyKeymap` + `searchKeymap` + `foldKeymap`; **`indentWithTab` is not installed** — Tab leaves the editor. `Escape` in the search panel closes it, `Escape` in the editor is left to the page (the peek panel's close, `PeekPanel.svelte:105`) | A `contenteditable` that eats Tab is a keyboard trap; WCAG 2.1.2 and the peek panel's own focus discipline (spec 008) both forbid it. Authors indent with Format (Decision 8), not Tab. Escape is already a page-level gesture in every place this component mounts, and a payload must not swallow it. |
| 8 | **2026-09-03** — `JsonEditor` reports `valid` (the text parses) and shows the parse error as a **lint diagnostic at the offending position** (`@codemirror/lint`, position derived from `JSON.parse`'s message where the engine gives one, else the end); a **Format** action pretty-prints a valid document in place and does nothing to an invalid one. Nothing is saved by the component — the consumer owns the write | Spec 016's item editor cannot post text that does not parse (spec 014 strict decoding would 400 it), and an error that says only "invalid JSON" sends the author hunting. The engine's error message is the only position source without a second parser, and where it has none, the end of the document is honest. Format is the one transform an author asks for; anything else (sorting keys, quoting) would be the component deciding what the document means. |
| 9 | **2026-09-03** — The UI application-line budget is **14,000** (owner decision 2026-09-03), amending design §8 and spec 009 #11 / spec 010 #7; the test count stays reported beside it under no ceiling. This spec's own DoD is the number it lands at, reported in the PR beside `main`'s; spec 016 must fit under 14,000 with it | The ceiling was set against growth without features (spec 009 #11's "the next feature argues with §8"), and spec 016 is six screens, an editor and a comparison — the argument §8 asked for. Raised here rather than in 016 because this spec is the first to need the number written down (a viewer plus an editor replaces 219 lines with more), and because a budget raised by the spec that spends it is no budget. 14,000 is `main`'s 6,678 plus a measured estimate for 016's screens with a margin for one review cycle, not a round number. |
| 10 | **2026-09-03** — CodeMirror is imported **statically**; the built bundle is measured before and after and the PR states both; the ceiling for the whole `dist` after this spec is **1.2 MB** uncompressed. No `manualChunks` | The viewer sits on the default route (`/` → `/traces`), and the trace peek is the first thing a reader opens, so a dynamic import would only move the wait from load to first click and add the first `import()` to a codebase that has none (`vite.config.ts` has no chunking today). The number is stated so a later spec can argue with it; 1.2 MB is roughly today's 664 KB plus what the eight packages cost minified. |
| 11 | **2026-09-03** — Small records stay records: an observation's `usage`, `cost_details` and `model_parameters` (`ObservationDetail.svelte:130-137`) render as a two-column **key/value list**, not in a CodeMirror instance; only the three payloads (`input`, `output`, `metadata`) and the trace's own metadata are documents | Six editor instances per observation panel is a cost with no reader on the other side: nobody searches a `usage` block, and a mounted CodeMirror is a `contenteditable` with a gutter and a keymap. A record of ten scalars reads better as a list than as JSON. The line between them is *depth and size* — a record is flat and short — and is written in the component's doc comment. |
| 12 | **2026-09-03** — A payload that is not JSON at all (the API can return a bare string for a text input) is shown as **plain text**, no JSON language, same surface | `gen_ai.prompt` on a plain span is a string, and the old tree already special-cased it (`JsonNode.svelte:54-55`). Wrapping it in quotes to make it JSON would put escapes on every newline of a prompt; the reader wants the prompt. |
| 13 | **2026-09-04** — A failed `/observations/{id}/io` is reported by the **owner**, above the payloads, and the banner stays a live retry; `Payload` gains no `failure` prop | Amends the "`/io` fails" edge case below, which asks the banner to show the failure and calls it "`Payload`'s existing failure path" — `Payload` has none, the failure has always lived in `ObservationDetail` (`:139-143`), and the Component contract freezes `Payload`'s props two paragraphs earlier. What the edge case actually promises is kept: the preview stays, nothing is thrown away, and pressing the banner again is the retry. Moving the message into the banner would spread one observation's fetch state across three components instead of one. |
| 14 | **2026-09-04** — The search panel's fields are styled from the same **tokens** the app's form classes spend (`--color-canvas`, `--color-border`, `--radius-md`, `--text-sm`) rather than from the classes themselves, and CodeMirror's own `.cm-button` gradient is turned off | Amends the Theme contract below, which asked for the classes. CodeMirror builds those inputs itself, inside a panel this component never renders, so there is no element to put a class on; the alternative — an `@apply` rule for CodeMirror's selectors in `app.css` — would put one component's markup in the token file. The gradient is not cosmetic: it is a pale plate that in dark mode carries light text, and reads as three empty buttons (found in this spec's Chrome run). |
| 15 | **2026-09-04** — `valid` is the **linter's** answer, not a parse of its own: the diagnostic and the flag come from one `JSON.parse` per pause (the linter's 300 ms), so a consumer reads `valid` as settling shortly after the last keystroke rather than during it. It is answered synchronously once, on mount. `disabled` does not gate it, and does not rebuild the editor either — it is an `EditorState.readOnly` reconfigured through a `Compartment` | Two sources for one question is two answers: the shape this amends parsed the whole document on every keystroke *and* let the linter parse it again 300 ms later, and gated both on `disabled`, so a disabled editor reported the validity of text it no longer held. Decision 8 is unchanged — the error is still a diagnostic at the engine's position — but this is what spec 016 must assume of the prop: gate a Save on it, and re-check on submit rather than trust it within a keystroke of typing. Rebuilding the view on `disabled` (which passing it to the mounting attachment did) discarded the cursor, the undo history, an open search panel and every fold opened by hand. |
| 16 | **2026-09-04** — The fold walk runs **after the first paint** (`requestAnimationFrame`), and its parse budget is **400 ms** rather than a second. The budget is a ceiling for the document that cannot be folded in time, not a price the ordinary one pays | The first change is the one that matters and is free: the walk parses the whole payload to find what to fold, and doing that before the first frame is what kept a panel blank while it ran. The budget was then measured across 150/250/300/350/500/1000 ms on a 3.9 MB payload, and the curve is a step, not a slope — below ~275 ms the parse aborts and the document opens with ~5 of its folds instead of ~53 (blocking ~155 ms at 150, ~255 ms at 250), and above it the parse completes and blocking is flat at ~330 ms whatever the budget. So a budget over the knee costs nothing for a payload that fits and only decides where folding gives up; 400 ms keeps 3.9 MB navigable with headroom for a slower machine, and bounds a panel's four instances at 1.6 s rather than the 4 s a second each allowed. Decision 4 calls folding what makes a megabyte navigable, and 150 ms would have broken that promise at exactly the size it was written for. Below ~1 MB none of it moves, and the corpus's 3.8 KB payload never produced a long task at all. |

## Component contract

`$lib/components/json/CodeArea.svelte` (internal), `JsonView.svelte`,
`JsonEditor.svelte`:

```ts
// JsonView
{ value: unknown; label: string; folded?: boolean /* Decision 4 */ }

// JsonEditor
{ text: string /* bindable */; label: string;
  valid?: boolean /* bindable, out */; disabled?: boolean }
```

`label` is the accessible name (`aria-label`) of the editing region. Both
expose a **Copy** (whole document) and the editor a **Format** action in
their toolbar; `JsonView` on a marker shows the Decision 3 banner above the
toolbar. Height grows with content up to a viewport-relative cap, then
scrolls inside; the page never scrolls horizontally (design §8).

`Payload.svelte` keeps its props and its three branches (marker / refused /
present); the present branch renders `JsonView`. `ObservationDetail` keeps
its three `Payload`s and turns the three records into the key/value list of
Decision 11. `TraceDetail` renders the trace's metadata with `JsonView`.

## Theme contract

`app.css` gains `--color-code-key` and `--color-code-punct` beside the two
code colours, as `light-dark()` pairs; the CodeMirror `HighlightStyle` maps
`tags.propertyName` → key, `tags.string` → string, `tags.number` /
`tags.bool` / `tags.null` → number, punctuation → punct. Editor chrome
(gutter, fold markers, selection, search panel, active line) uses
`--color-surface` / `--color-raised` / `--color-border` / `--color-accent-soft`
/ `--color-muted`. The search panel's inputs use the app's form classes.

## Testing

- **Vitest, CodeArea**: a value round-trips through the viewer's text;
  a document over 400 lines opens folded at depth 2 and a short one flat;
  a bare string renders without the JSON language; the editor reports
  `valid` false with a diagnostic at the error position for `{"a": }` and
  true after the fix; Format pretty-prints and leaves an invalid document
  alone; Tab moves focus out of the editor; copy writes the whole document.
- **Vitest, Payload**: the marker branch shows the preview text and the
  banner, the banner calls `onload`, the loaded value replaces the preview
  (`Payload.test.ts` rewritten, same cases).
- **Vitest, contrast**: the two new tokens pass ≥ 4.5:1 on every surface in
  both themes.
- **Lines and bytes**: `make ui-lines` under 14,000; `dist` under 1.2 MB;
  both numbers in the PR beside `main`'s.
- **e2e** (`json.spec.ts`, over the 4 KiB budget of `global-setup.ts:17-30`):
  the large-payload fixture shows the banner, loading it shows the whole
  document (a key from past the cut is on screen after scrolling); `Cmd-F`
  finds a string that is not rendered before the search; the fold gutter
  folds and unfolds; a long prompt string wraps without a horizontal
  scrollbar at 375 px; both themes render the four code colours (computed
  style differs between `data-theme` values). `smoke.spec.ts:91-104`
  rewritten to the banner.
- **Chrome (DoD)**: trace peek and full page, the large-payload trace and a
  plain-string input, both themes, 375 px, console clean.

## Edge cases

- **Marker with no `preview`** (spec 004 #25 omits it when nothing fit):
  the banner alone, no document.
- **`/io` fails**: the preview stays, the banner shows the failure and
  offers a retry — `Payload`'s existing `failure` path.
- **A value that stringifies to more than ~10 MB**: shown; CodeMirror
  handles it, folding is what makes it navigable; no cap is added here
  because the server's body cap already bounds it.
- **Observation changes while the panel is open**: the editor state is
  recreated for the new value (keyed on observation id, as `ObservationDetail`
  already keys its `full` state, `:25-29`).
- **Reduced motion**: no animated fold; the app's `prefers-reduced-motion`
  rule covers CodeMirror's transitions.

## Config additions

None.

## Out of scope

The eval screens and the editor's first consumer (016); syntax modes other
than JSON and plain text; diffing two documents side by side (016's compare
shows them beside each other, not as a diff); a JSON *schema* for item
bodies (spec 014 #4 keeps them opaque); editing a trace's payloads (they are
immutable).
