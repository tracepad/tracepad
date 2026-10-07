"""Every anchor of a link into the site's latest/ or dev/ is an id its page has
(spec 050 #19): the binary and the skill print these links, and a person
follows them in the middle of an upgrade.

The ids are Python-Markdown's own, with mkdocs.yml's extensions, not a copy of
its slug rule: `A — B` is `a-b` on the site and `a--b` on GitHub, and a repeat
is `_1` there. scripts/site-links.sh checks the page; this, its anchor.

Run by `make docs-site-test`, through the same locked environment as the build.
"""

import os
import re
import subprocess
import unittest

import markdown
from mkdocs.config import load_config

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
FIXTURE = os.path.join(ROOT, "scripts", "site-links-fixture")
# A period after the anchor ends the sentence, not the link: no id has one.
LINK = re.compile(
    r"tracepad\.github\.io/tracepad/(?:latest|dev)/([A-Za-z0-9_./-]*)#([A-Za-z0-9_-]+)"
)


CONFIG = load_config(config_file=os.path.join(ROOT, "mkdocs.yml"))


def ids(path):
    """The ids the site gives the headings of the page at path."""
    md = markdown.Markdown(
        extensions=CONFIG["markdown_extensions"], extension_configs=CONFIG["mdx_configs"]
    )
    with open(path, encoding="utf-8") as f:
        md.convert(f.read())
    found, tokens = [], list(md.toc_tokens)
    while tokens:
        token = tokens.pop(0)
        found.append(token["id"])
        tokens[:0] = token["children"]
    return found


def page(docs, name):
    name = name.strip("/")
    return os.path.join(docs, (name or "index") + ".md")


class SiteAnchorTest(unittest.TestCase):
    def test_the_rule_is_the_sites(self):
        got = ids(page(FIXTURE, "install/"))
        self.assertEqual(
            got, ["installing", "upgrading", "upgrading_1", "the-serve-command-and-its-flags"]
        )

    def test_every_anchor_names_a_heading(self):
        files = (
            subprocess.run(["git", "ls-files", "-z"], cwd=ROOT, check=True, capture_output=True)
            .stdout.decode()
            .split("\0")
        )
        known, seen = {}, 0
        for name in files:
            if not name or name.startswith("scripts/site-links"):
                continue
            try:
                with open(os.path.join(ROOT, name), encoding="utf-8") as f:
                    text = f.read()
            except (UnicodeDecodeError, IsADirectoryError, FileNotFoundError):
                continue
            for target, anchor in LINK.findall(text):
                seen += 1
                md = page(os.path.join(ROOT, "docs"), target)
                if not os.path.isfile(md):
                    continue  # site-links.sh says so
                if md not in known:
                    known[md] = ids(md)
                self.assertIn(anchor, known[md], f"{name}: …/{target}#{anchor}")
        # The binary and the install script print three; none found would
        # pass anything.
        self.assertGreaterEqual(seen, 3)


if __name__ == "__main__":
    unittest.main()
