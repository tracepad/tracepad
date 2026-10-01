"""Build-time hooks for the documentation site (spec 050 #5).

docs/ links out of itself in four places — `../README.md#getting-it`,
`../CHANGELOG.md`, `../scripts/smoke` — and on GitHub those are right as they
are, and `make doc-anchors` reads the first of them. On the site the target is
not a page, and `mkdocs build --strict` would refuse the build. The files stay
as they are and the hook points each such link at the same path on GitHub, for
the build only. A link whose target exists nowhere is left alone, so that
strict mode still reports it.
"""

import os
import re

REPO = "https://github.com/tracepad/tracepad"

# `](../path)` and `](../path#anchor)`. Pages sit directly under docs/, so one
# `../` is the repository root.
LINK = re.compile(r"\]\(\.\./([^)#\s]+)(#[^)\s]*)?\)")


def on_page_markdown(markdown, page, config, files):
    root = os.path.dirname(os.path.abspath(config["config_file_path"]))

    def to_github(match):
        path, anchor = match.group(1), match.group(2) or ""
        target = os.path.join(root, path)
        if os.path.isdir(target):
            kind = "tree"
        elif os.path.isfile(target):
            kind = "blob"
        else:
            return match.group(0)
        return f"]({REPO}/{kind}/main/{path}{anchor})"

    return LINK.sub(to_github, markdown)
