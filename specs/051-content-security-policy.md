# Spec 051 — A policy for the page: what script, style and requests the interface may run

**Status:** ✅ SHIPPED
**Sprint:** October 2026

> Spec 001 #14 gave every response `frame-ancestors 'none'` and left the rest
> of a Content-Security-Policy to a spec of its own, because a `script-src`
> "has to be written against what the interface's bundle actually loads and
> tested screen by screen". This is that spec. The interface renders text it
> did not write: prompts and completions from an LLM, tool arguments, whatever
> an application put into a span's metadata. Today nothing in it turns that
> text into markup (there is no `{@html}` and no Markdown renderer), so the
> surface is clean. A policy is what keeps it clean when someone adds one
> next year, or when a dependency does. Without one, the first injected
> `<img onerror>` runs with the session's cookie beside it, on the origin that
> holds every trace, every key and the Delete buttons. This spec gives the two
> HTML documents the server hands out a policy that allows the bundle's own
> files and the one line that starts them, and nothing else, and it makes the
> end-to-end suite fail on any refusal, so the policy cannot rot into a list
> of exceptions or into a blank page nobody noticed.

---

## Overview

One PR. Deliverables:

- **The policy** (Decisions 1–6): `internal/server/csp.go` builds a
  `Content-Security-Policy` from the HTML document it will send, and
  `serveUIDocument` sends it in place of the `frame-ancestors 'none'` every
  response carries. Nothing else about the response changes.
- **The proof** (Decisions 7–8): every end-to-end test runs under a watch that
  fails it on a `securitypolicyviolation`, a new `csp.spec.ts` checks what is
  sent, that the policy is enforced, and that every screen of the interface
  opens clean, in both browsers.
- **Docs** (*Docs to touch*): the response-headers table, the interface page,
  and a line for whoever fronts the server with a proxy.

Not here (*Out of scope*): Trusted Types, reporting, `Permissions-Policy`,
`Cross-Origin-*` headers, HSTS.

Builds on spec 001 #14 (the four headers, set by one middleware, and the
sentence this spec answers), spec 006 #1 and #9 (the SPA and its stub), spec
015 (the editor), spec 041 #15 (a media body's own sandbox and the pictures
the interface draws from blobs) and spec 028 (the session cookie the policy
protects).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-10-01** — **The policy is the HTML documents'.** Two responses are documents: the SPA's entry, which every client-side route and `/index.html` answer with, and the stub a build without the interface serves. Each is sent under the policy of the *Contract* section and nothing else. Every other response — the API, the bundle's files, a media body — keeps what it had: spec 001 #14's `frame-ancestors 'none'`, or the media sandbox of spec 041 #15 | A policy governs the document it arrives with and what that document loads; a JSON body or a `.js` file sent under one is not constrained by it, and the browser that matters reads the document's. So the rule that every response says `frame-ancestors 'none'` stays true and stays one line in one middleware (`withResponseHeaders`), and the two handlers that serve documents replace it, as the media handler already does (#4). A `default-src 'none'` on JSON answers was considered and left out: it constrains nothing a JSON response does, `nosniff` already keeps it from being read as anything else, and it would make a browser's raw view of an API answer the one place the policy shows. |
| 2 | **2026-10-01** — **Script is `'self'` and the hash of each inline script, and nothing else.** `script-src 'self' 'sha256-…'`: no `'unsafe-inline'`, no `'unsafe-eval'`, no `'strict-dynamic'`, no scheme or host source. What the bundle loads, measured on the built interface: every chunk is a same-origin module file under `/_app/immutable/`, pulled by `<link rel="modulepreload">` and by `import()`; the only inline code is the entry's one `<script>`, the four lines that name the two entry chunks and call `kit.start`; no chunk calls `eval` or `new Function` (a search of the built output finds neither); and the whole end-to-end suite runs under the policy without one refusal (#7) | This is the line the policy exists for. `'self'` lets the bundle run and no script anybody can make the page *contain* run. Leaving `'unsafe-inline'` out is what makes an injected `<script>` or `onerror="…"` inert; leaving `'unsafe-eval'` out closes `new Function` and string timers, the second thing an injection reaches for. `'strict-dynamic'` is for pages that load scripts they cannot list, and this one lists all of them. The hash is the way to keep the one inline script without opening the door to every other: a hash allows exactly one text. |
| 3 | **2026-10-01** — **The hash is taken at start, from the bytes the server will send, by the server.** `documentPolicy` reads the entry's `<script>` elements (ignoring comments and elements with a `src`), hashes each body with SHA-256 and writes them into `script-src`. A bundle whose entry gains a second inline script gets a second hash with no one touching the policy; a rebuild that changes the first one changes its hash with it. **SvelteKit's `kit.csp` in `hash` mode was the first choice and is not used.** It computes the same hashes at build time and, for an SPA fallback page, writes them into a `<meta http-equiv="content-security-policy">` in `index.html`. That is the wrong place for them: a meta policy cannot carry `frame-ancestors`, `form-action` or `sandbox`, so the header spec 001 #14 requires would still have to exist, and the Go side would read the hash back out of the tag — a second parser for the same fact — or keep a copy of it. A nonce was rejected for the same reason in the other direction: it needs the document rewritten on every request, which makes its inline script unhashable | The policy has to agree with the document byte for byte, and the server is the one party that holds both: the policy it sends and the file it embeds. One function over the served bytes cannot drift; a hash written by a build step and read by another program can, and the day it does the interface is a blank page whose only symptom is a console line. The cost is a regular expression over one small file, once, at start, and a test that recomputes the hash with another implementation (`csp.spec.ts`) so the two cannot agree on a mistake. Moving the bootstrap into a file instead would need SvelteKit to emit one, which `adapter-static` does not. |
| 4 | **2026-10-01** — **A document's policy replaces the base header; there is never a second.** `withResponseHeaders` still sets `frame-ancestors 'none'` first. `serveUIDocument` sets the document's policy with `Header().Set`, so the response carries exactly one `Content-Security-Policy`. The entry is a document however it was asked for: a request for the file named `index.html` — `/index.html`, and `/index.html/` — is answered by `serveIndex` and not by the file server. The policies are computed once, at start (`useBundle`, and the stub's in `New`) | Two `Content-Security-Policy` headers are two policies, and the browser enforces both, so one that said less would not loosen the other and one that said more would be dead text. One header is also what spec 001 #14 promised and `TestEveryResponseRefusesToBeFramed` holds. The file branch is a real hole and not a precaution: `http.ServeFileFS` answers `/index.html` with a redirect to `./`, but `/index.html/` — a trailing slash — reaches the same file, and it served the entry as a plain file with only the base header: a document with no `script-src`, on a path anybody can type. The test asks for both spellings. |
| 5 | **2026-10-01** — **Style is `'self' 'unsafe-inline'`, and only style.** The policy cannot be strict about style, and measured why. With `style-src 'self'` the interface produced refusals from three places: CodeMirror 6, which writes its theme into a `<style>` element (its style library uses a constructable stylesheet only inside a shadow root, and this editor is in the document); Svelte, whenever a component sets `style` on an element, such as the progress bars' widths; and bits-ui, which saves and restores `<body>`'s `style` attribute around a dialog's scroll lock. The first is an element and the other two are attributes. A hash or a nonce names an element's text and cannot name an attribute, and the text of CodeMirror's sheet is not known until it runs. So style keeps `'unsafe-inline'`, and the rest of the policy is what makes that cheap (#6) | What an attacker does with style injection is read the page or send what it read somewhere: a background `url()`, an `@import`, a font. Every one of those is a fetch, and every fetch directive here is `'self'` (and `data:` and `blob:` for pictures). Style that cannot reach another origin can restyle the page, which is a nuisance and not the theft `script-src` prevents. Rewriting the three sources to avoid inline style was weighed and left: Svelte's `style=` is the framework's way to put a computed number on an element, CodeMirror's loading is a library's, and a fork or a patch of a dependency for a nuisance is the wrong trade. The line to hold is in the test: `script-src` carries no `unsafe-inline`, and the test fails if it ever does. |
| 6 | **2026-10-01** — **What the page may fetch, directive by directive.** `default-src 'none'` is the floor, so a kind of load the policy does not name is refused and the next dependency that wants one makes a test fail instead of making a hole. `connect-src 'self'`: the interface talks to its own API and no other server, and a credential it holds cannot be posted elsewhere by script that somehow ran. `img-src 'self' data: blob:`: `data:` for the empty icon `app.html` names (a browser reads an icon under `img-src`), `blob:` for the pictures the interface draws from bytes it fetched with the session's credentials (spec 041 #15). `font-src 'self'`: the two typefaces are files in the bundle. `worker-src 'none'`, `base-uri 'none'` and `form-action 'self'` because `default-src` does not cover them. `frame-ancestors 'none'` as before. There is no `media-src`, `object-src`, `frame-src` or `manifest-src`: nothing in the interface plays audio or video, embeds or frames anything, and the floor refuses all of them | Each of these was written against something the bundle does, and the ones it does not do are absent, not allowed "in case". `form-action` and `base-uri` are the two a `default-src` of `'none'` still leaves to the document, and both are how an injected form or `<base>` redirects a login. `blob:` is the one that needs a sentence: a blob is a URL the page made from bytes it already has, so it adds no origin. `data:` is an image only; a `data:` script is refused by `script-src`. |
| 7 | **2026-10-01** — **Every end-to-end test runs under a watch that fails it on a violation.** `ui/tests/e2e/fixtures.ts` replaces `@playwright/test`'s `test` in every spec. An automatic fixture listens, in every page and frame of the test's browser context, for `securitypolicyviolation`, and fails the test at its end listing what was refused and by which directive, from which script. The watch is not allowed to pass for the wrong reason: it also fails any HTML document the browser received without a `script-src`, because a policy that is not sent produces no violations either. A test that provokes a refusal on purpose collects it with `take()`, which is the only way one is not a failure | The end-to-end suite is already some 460 runs of the interface — about 230 scenarios, in a desktop and a phone-sized browser — and between them they open every dialog, panel, editor, chart and picker. Making them the sweep costs one fixture and finds a refusal the moment a change causes it, in the test of the screen it broke. A policy checked by one test of its own would find only what that test visited, and the first screen it missed would be the one that shipped broken. The second half is the lying-check rule: absence of violations proves nothing about a policy that was never in force (spec 041 #15's sandbox is tested the same way, by checking the header and then the effect). |
| 8 | **2026-10-01** — **A sweep that cannot miss a screen, and tests that the policy bites.** `csp.spec.ts` opens every route that has a screen, signed in as an owner of a project with a dataset, runs, a prompt, a queue and the corpus in it, and the three that are not behind a session as nobody; its list is compared with the `+page.svelte` files on disk, so a new screen fails the next run until it is listed. Beside it: the header on the entry (`/`, a deep link, `/index.html` and `/index.html/`) is exactly one, its `script-src` is `'self'` and the SHA-256 of the entry's one inline script computed in the test, and it holds no `'unsafe-eval'`; and five things an injection would try — an inline script and an inline handler, a script from another origin, `new Function`, a request to another origin, a form posted elsewhere — are each refused, by the directive that should refuse them, and have not run. In Go: the policy for each shape of document (one script, two, a `src`, a comment, none), the floor (#6's directives, no unsafe source in `script-src`, ten directives and no more), and the entry, however asked for, under exactly one policy | The suite's watch (#7) shows that nothing is refused; these show that something would be. A refusal test that only inspected the header would pass on a policy the browser then ignored (a typo in a directive name is silently dropped), so the second group asks the browser. The route list is the check that "every screen" stays true: the interface's routes are files, so the files are what is compared. |
| 9 | **2026-10-01** — **No reporting, no report-only, no setting.** The policy is enforced from the first byte, without `report-uri`, `report-to` or a `Content-Security-Policy-Report-Only` stage, and there is no environment variable or flag to loosen or remove it. The development server (`npm run dev`) is Vite's and sends none | A self-hosted server has no collector to report to, and one it would have to run is a new service for a header. A refusal is a console line for the person at the screen, which is where a bug in the interface is found, and the suite is where it is found before release. A report-only stage is for shipping a policy to traffic nobody can test; here the whole interface is tested before it is shipped, and a policy that is never enforced is not one. A switch to turn it off is a switch to have no policy: an operator who needs a different one puts it on the proxy, where it will be intersected with this one (the browser enforces both) or replace it. |

## Contract

The policy of a document that carries one inline script, as the server sends
it (one header, one line; broken here for reading):

```
Content-Security-Policy:
    default-src 'none';
    script-src 'self' 'sha256-<hash of the inline script>';
    style-src 'self' 'unsafe-inline';
    img-src 'self' data: blob:;
    font-src 'self';
    connect-src 'self';
    worker-src 'none';
    base-uri 'none';
    form-action 'self';
    frame-ancestors 'none'
```

- **Which responses**: the SPA entry (every client-side route, and
  `/index.html`) and the stub of a build without the interface. The stub has
  one `<style>` and no script, so its `script-src` is `'self'` alone.
- **Which hashes**: one for each `<script>` element of the document that has a
  body, taken at start from the bytes served. A script with a `src`, and a
  script inside an HTML comment, have none.
- **What does not change**: the other three headers of spec 001 #14, the base
  header on every other response, the media sandbox, `Cache-Control: no-cache`
  on the documents.

## Testing

- **The policy as a table** (#3): one inline script, attributes and capitals
  on its tag, two scripts (two hashes, in document order), a `src` script, a
  script in a comment, none.
- **The floor** (#2, #5, #6): `default-src 'none'`; no `'unsafe-inline'`,
  `'unsafe-eval'`, `'strict-dynamic'`, `*`, `http:` or `https:` in
  `script-src`; each directive as the contract states it; ten directives and
  no more.
- **The documents** (#1, #4): `/`, a deep link, an unknown path,
  `/index.html` and `/index.html/` each answer under exactly one policy with the hash of the
  script served; a bundle file, `/health`, `/api/v1` and an API miss keep
  `frame-ancestors 'none'` alone; the stub is under its policy; the existing
  hardening test still finds one policy with `frame-ancestors 'none'` on every
  response and the media sandbox intact.
- **The header and the page agree** (#3, #8, e2e): the entry's `script-src` is
  `'self'` plus the hash the test computes itself.
- **Enforced** (#8, e2e): an inline script and an inline handler are refused
  (`script-src-elem`, `script-src-attr`) and have not run; a script from
  another origin is refused; `new Function` throws `EvalError`; a `fetch` to
  another origin is refused by `connect-src`; a form to another origin by
  `form-action`.
- **Every screen** (#7, #8, e2e): the 28 screens signed in and the three
  signed out, plus an address with no screen, with no refusal, at a desktop
  and at a phone's width; the list equals the route files; and the rest of the
  suite, which now fails on a refusal anywhere in it.
- **Live** (before the push): the interface in Chrome against a server on a
  fresh database with the corpus in it — the dashboard with its five charts,
  a trace with both editors, the Score dialog opened and dismissed with the
  body's scroll lock restored, the server settings — console open, and no line
  about the policy; and the icon checked to obey `img-src`, which is why
  `data:` is in it (#6).

## Edge cases

- **A tab open across an upgrade** holds the old document under the old policy
  until it reloads; the document is `no-cache`, so the reload is the new one.
- **A proxy that adds its own `Content-Security-Policy`** adds a second policy
  and the browser enforces both: it can narrow this one and never loosen it. A
  proxy that *replaces* the header owns the page's policy from then on, and
  the interface is its to keep working under it ([docker.md](../docs/docker.md)).
- **An unknown path under `/_app/`** is a `404` and not the document (spec 006
  #1 stands), so it is not a document with a policy either.
- **`blob:` pictures in a tab the interface opens** (spec 041 #15): an
  `about:blank` window inherits the opener's policy, so the image in it is
  drawn under `img-src … blob:` and nothing else in it runs.
- **A server behind another host name** is the same origin to the browser that
  reached it, so `'self'` is whichever name it was reached by.
- **A build without the interface** has no bundle to name, and the stub's
  policy is the same function's over its one style element.

## Docs to touch

- `docs/api.md`: the response-headers table says what the interface's pages
  carry beside the one line every response does.
- `docs/ui.md`: *What the page may load* — the policy in words, why style is
  the exception, what to do when a change is refused (read the console, add
  nothing to the policy without a Decision here).
- `docs/docker.md`: one sentence under *Serving over TLS* — a proxy's own
  `Content-Security-Policy` is enforced with this one.
- `CHANGELOG.md`, `AGENTS.md` (the status block).

## Out of scope

- **Trusted Types** (`require-trusted-types-for 'script'`): the interface
  writes no HTML from strings today, so there is nothing for it to check yet;
  it is the next tightening if one is ever written.
- **A policy with no `'unsafe-inline'` for style** (#5): needs the editor to
  render into a shadow root, Svelte's style handling to change, and a patch to
  the dialogs' scroll lock; the exposure it would remove is already closed by
  #6.
- **Reporting and report-only** (#9).
- **`Permissions-Policy`, `Cross-Origin-Opener-Policy` and the other
  cross-origin headers, and HSTS**: separate questions with separate
  trade-offs (HSTS in particular belongs to whoever terminates TLS, and the
  server does not).
- **A policy on the API's responses** (#1).
