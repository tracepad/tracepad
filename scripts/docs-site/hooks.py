"""Build-time hooks for the documentation site (spec 050 #5).

docs/ links out of itself in four places — `../README.md#getting-it`,
`../CHANGELOG.md`, `../scripts/smoke` — and on GitHub those are right as they
are, and `make doc-anchors` reads the first of them. On the site the target is
not a page, and `mkdocs build --strict` would refuse the build. The files stay
as they are and the hook points each such link at the same path on GitHub, for
the build only. A link whose target exists nowhere is left alone, so that
strict mode still reports it.

Inline links and reference definitions are rewritten; fenced code is not,
because a `](../x)` in an example is prose about a link.
"""

import os
import re

REPO = "https://github.com/tracepad/tracepad"

# `](../path)` and `](../path#anchor)`; and a reference definition,
# `[id]: ../path#anchor`. Pages sit directly under docs/, so one `../` is the
# repository root.
INLINE = re.compile(r"\]\(\.\./([^)#\s]+)(#[^)\s]*)?\)")
DEFINITION = re.compile(r"^(\s{0,3}\[[^\]]+\]:\s*)\.\./([^#\s]+)(#\S*)?(?=\s|$)")
FENCE = re.compile(r"^\s*(```|~~~)")


def on_page_markdown(markdown, page, config, files):
    root = os.path.dirname(os.path.abspath(config["config_file_path"]))

    def url(path, anchor):
        target = os.path.join(root, path)
        if os.path.isdir(target):
            kind = "tree"
        elif os.path.isfile(target):
            kind = "blob"
        else:
            return None
        return f"{REPO}/{kind}/main/{path}{anchor or ''}"

    def inline(match):
        found = url(match.group(1), match.group(2))
        return match.group(0) if found is None else f"]({found})"

    def definition(match):
        found = url(match.group(2), match.group(3))
        return match.group(0) if found is None else match.group(1) + found

    lines = markdown.split("\n")
    fenced = False
    for i, line in enumerate(lines):
        if FENCE.match(line):
            fenced = not fenced
            continue
        if fenced:
            continue
        line = INLINE.sub(inline, line)
        lines[i] = DEFINITION.sub(definition, line)
    return "\n".join(lines)
