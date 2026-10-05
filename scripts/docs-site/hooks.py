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

After the build, the site is also written for agents (spec 053 #14): every
page's Markdown beside its HTML (`quickstart.md` next to `quickstart/`), with
the same links rewritten, so relative links between pages resolve as Markdown;
`llms.txt` in llmstxt.org's form, from the navigation; `llms-full.txt`, every
page in that order; and `install.sh`, the install script (spec 053 #3). They
are files of the version being built; `deploy.sh` mirrors them at the root.
"""

import os
import re
import shutil

REPO = "https://github.com/tracepad/tracepad"

# `](../path)` and `](../path#anchor)`; and a reference definition,
# `[id]: ../path#anchor`. Pages sit directly under docs/, so one `../` is the
# repository root.
INLINE = re.compile(r"\]\(\.\./([^)#\s]+)(#[^)\s]*)?\)")
DEFINITION = re.compile(r"^(\s{0,3}\[[^\]]+\]:\s*)\.\./([^#\s]+)(#\S*)?(?=\s|$)")
FENCE = re.compile(r"^\s*(```|~~~)")

# Each page's Markdown as this hook handed it to the build, by its path under
# docs/: the agents' copy is that text, not a second pass over the file
# (spec 053 #19). Emptied at the start of every build, `mkdocs serve`'s too.
BUILT = {}


def on_pre_build(config):
    BUILT.clear()


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
    markdown = "\n".join(lines)
    if page is not None:
        BUILT[page.file.src_uri] = markdown
    return markdown


# --- The site for agents (spec 053 #14) -------------------------------------

LINK_TEXT = re.compile(r"\[([^\]]*)\]\([^)]*\)")
SENTENCE_END = re.compile(r"(?<=[.!?])\s")


def nav_pages(nav, section=None):
    """(section, title, path) for every page of the navigation, in order.

    A page at the top level has no section; a link out of the site (the
    changelog) is not a page and is left out."""
    for entry in nav or []:
        if isinstance(entry, str):
            entry = {None: entry}
        for title, value in entry.items():
            if isinstance(value, list):
                yield from nav_pages(value, title)
            elif "://" not in value:
                yield section, title, value


def description(markdown):
    """The page's first sentence of prose: past the title, a fenced block and
    a heading, one line, with links reduced to their text."""
    fenced = False
    paragraph = []
    for line in markdown.split("\n"):
        if FENCE.match(line):
            fenced = not fenced
            continue
        if fenced:
            continue
        stripped = line.strip()
        if not stripped:
            if paragraph:
                break
            continue
        if stripped.startswith(("#", "<", "!", "|", ">")) and not paragraph:
            continue
        paragraph.append(stripped)
    text = LINK_TEXT.sub(r"\1", " ".join(paragraph))
    first = SENTENCE_END.split(text, maxsplit=1)[0]
    return first if len(first) <= 200 else first[:197].rstrip() + "…"


def on_post_build(config):
    root = os.path.dirname(os.path.abspath(config["config_file_path"]))
    site = config["site_dir"]
    base = config["site_url"] or ""
    if base and not base.endswith("/"):
        base += "/"

    pages = []
    for section, title, path in nav_pages(config["nav"]):
        markdown = BUILT.get(path)
        if markdown is None:
            # A dirty build (`mkdocs serve --dirty`) hands only the pages it
            # rebuilt to on_page_markdown; the rest are read as the build would.
            with open(os.path.join(config["docs_dir"], path), encoding="utf-8") as f:
                markdown = on_page_markdown(f.read(), None, config, None)
        target = os.path.join(site, path)
        os.makedirs(os.path.dirname(target), exist_ok=True)
        with open(target, "w", encoding="utf-8") as f:
            f.write(markdown)
        pages.append((section, title, path, markdown))

    lines = [
        f"# {config['site_name']}",
        "",
        f"> {config['site_description']}",
        "",
        "Every page below is Markdown, and `llms-full.txt` beside this file is all of "
        "them in one. These addresses follow the newest release; a version's own "
        f"pages are under `{base}<version>/` (`dev` is the tip of `main`).",
        "",
        "A coding agent setting Tracepad up for a project starts at "
        f"[Agent setup]({base}agent-setup.md).",
    ]
    current = object()
    for section, title, path, markdown in pages:
        if section != current:
            lines += ["", f"## {section or 'Overview'}", ""]
            current = section
        lines.append(f"- [{title}]({base}{path}): {description(markdown)}")
    with open(os.path.join(site, "llms.txt"), "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")

    with open(os.path.join(site, "llms-full.txt"), "w", encoding="utf-8") as f:
        for i, (_, _, path, markdown) in enumerate(pages):
            if i:
                f.write("\n\n")
            f.write(f"<!-- {base}{path} -->\n\n{markdown.rstrip()}\n")

    shutil.copyfile(os.path.join(root, "scripts", "install.sh"), os.path.join(site, "install.sh"))
