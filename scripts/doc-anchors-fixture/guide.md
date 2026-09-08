# The anchor checker's own fixture

Every case in one file, so that a single `scripts/doc-anchors.sh --self-test`
asserts the whole rule (spec 026 #6). Two of the links below must be reported
and the rest must not, and the shell block near the bottom is what makes the
second one broken.

Nothing but the self-test reads this directory: the sweep itself covers
`docs/*.md`, `README.md` and `AGENTS.md`.

- A cross-file anchor to a heading the other file does not have:
  [the missing heading](other.md#the-missing-heading).
- A same-file anchor whose only heading is [inside a shell block](#running-the-server).
- A heading with backticks: [the `--dry-run` flag](#the---dry-run-flag).
- The second of two headings that read alike: [turning pages again](#turning-pages-1).
- A cross-file anchor that lands: [what the other file says](other.md#what-the-other-file-says).
- Prose *about* a link rather than a link: `[a label](nowhere.md#no-heading)`
  inside a code span, which is how `AGENTS.md` explains what is checked.

## The `--dry-run` flag

Punctuation other than hyphens and spaces goes, so the backticks leave nothing
behind and the three hyphens of `the---dry-run-flag` are the space plus the
flag's own two.

## Turning pages

The first of the two.

```sh
# Running the server
tracepad serve
```

The block above is not a heading, and the link that names it is the second
breakage this fixture asserts. A checker that reads fences resolves it and
reports one thing instead of two.

## Turning pages

The second of the two, which is `turning-pages-1`.
