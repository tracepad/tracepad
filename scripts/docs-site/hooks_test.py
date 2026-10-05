"""The hook that points docs/'s links out of itself at GitHub (spec 050 #5).

Run by `make docs-site-test`, through the same locked environment as the
build. The repository root is the real one, so `README.md` and `scripts/smoke`
are the files and the directory the hook looks for.
"""

import os
import tempfile
import unittest

import hooks

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
CONFIG = {"config_file_path": os.path.join(ROOT, "mkdocs.yml")}
GITHUB = "https://github.com/tracepad/tracepad"


def rewrite(text):
    return hooks.on_page_markdown(text, None, CONFIG, None)


class HookTest(unittest.TestCase):
    def test_a_file_and_its_anchor(self):
        self.assertEqual(
            rewrite("see [the README](../README.md#getting-it) first"),
            f"see [the README]({GITHUB}/blob/main/README.md#getting-it) first",
        )

    def test_a_directory_is_a_tree(self):
        self.assertEqual(
            rewrite("[smoke](../scripts/smoke)"),
            f"[smoke]({GITHUB}/tree/main/scripts/smoke)",
        )

    def test_two_links_on_one_line(self):
        out = rewrite("[a](../README.md) and [b](../CHANGELOG.md)")
        self.assertEqual(out.count(GITHUB), 2)

    def test_a_reference_definition(self):
        self.assertEqual(
            rewrite('[readme]: ../README.md#getting-it "the README"\n'),
            f'[readme]: {GITHUB}/blob/main/README.md#getting-it "the README"\n',
        )

    def test_fenced_code_is_left_alone(self):
        text = "```md\n[a](../README.md)\n[r]: ../README.md\n```\n~~~\n[b](../README.md)\n~~~"
        self.assertEqual(rewrite(text), text)

    def test_text_after_a_fence_is_rewritten_again(self):
        out = rewrite("```\nx\n```\n[a](../README.md)")
        self.assertIn(GITHUB, out)

    def test_a_target_that_exists_nowhere_is_left_for_strict_mode(self):
        text = "[a](../no-such-file.md#x) and\n[r]: ../no-such-dir/"
        self.assertEqual(rewrite(text), text)

    def test_links_inside_docs_and_external_ones_are_not_touched(self):
        text = "[a](install.md#upgrading) [b](https://example.org/../README.md) [c](#top)"
        self.assertEqual(rewrite(text), text)


AGENT_NAV = [
    {"Home": "index.md"},
    {"Get started": [{"Quickstart": "quickstart.md"}, {"Agent setup": "agent-setup.md"}]},
    {"Changelog": "https://github.com/tracepad/tracepad/blob/main/CHANGELOG.md"},
]
AGENT_PAGES = {
    "index.md": "# Home\n\nThe front page. More here.\n",
    "quickstart.md": "# Quickstart\n\nFrom [nothing](install.md) to a trace. Then more.\n\n"
    "See [the README](../README.md#getting-it).\n",
    "agent-setup.md": "# Agent setup\n\n```sh\ntracepad version\n```\n\n"
    "For an agent, not a person.\n",
}


class FakePage:
    """What the hook reads of MkDocs' page: the file's path under docs/."""

    def __init__(self, src_uri):
        self.file = type("File", (), {"src_uri": src_uri})()


class AgentSiteTest(unittest.TestCase):
    """What the build writes for agents (spec 053 #14)."""

    def build(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        site = os.path.join(tmp.name, "site")
        os.makedirs(site)
        hooks.on_pre_build(CONFIG)
        for name, text in AGENT_PAGES.items():
            hooks.on_page_markdown(text, FakePage(name), CONFIG, None)
        hooks.on_post_build(
            {
                **CONFIG,
                "site_dir": site,
                "site_url": "https://example.org/docs",
                "site_name": "Tracepad",
                "site_description": "One line about it.",
                "nav": AGENT_NAV,
            }
        )
        return site

    def read(self, site, name):
        with open(os.path.join(site, name)) as f:
            return f.read()

    def test_the_navigation_in_order_without_links_out(self):
        pages = list(hooks.nav_pages(AGENT_NAV))
        self.assertEqual(
            pages,
            [
                (None, "Home", "index.md"),
                ("Get started", "Quickstart", "quickstart.md"),
                ("Get started", "Agent setup", "agent-setup.md"),
            ],
        )

    def test_llms_txt(self):
        text = self.read(self.build(), "llms.txt")
        self.assertTrue(text.startswith("# Tracepad\n\n> One line about it.\n"))
        self.assertIn(
            "## Overview\n\n- [Home](https://example.org/docs/index.md): The front page.\n", text
        )
        self.assertIn(
            "## Get started\n\n"
            "- [Quickstart](https://example.org/docs/quickstart.md): From nothing to a trace.\n",
            text,
        )
        self.assertIn(
            "- [Agent setup](https://example.org/docs/agent-setup.md): "
            "For an agent, not a person.\n",
            text,
        )
        self.assertNotIn("CHANGELOG", text)

    def test_every_page_as_markdown_with_the_links_rewritten(self):
        page = self.read(self.build(), "quickstart.md")
        self.assertIn("[nothing](install.md)", page)
        self.assertIn(f"[the README]({GITHUB}/blob/main/README.md#getting-it)", page)

    def test_llms_full_txt_is_every_page_in_order(self):
        text = self.read(self.build(), "llms-full.txt")
        marks = [
            text.index(f"<!-- https://example.org/docs/{name} -->")
            for name in ("index.md", "quickstart.md", "agent-setup.md")
        ]
        self.assertEqual(marks, sorted(marks))
        self.assertIn("For an agent, not a person.", text)

    def test_the_install_script_is_in_the_site(self):
        with open(os.path.join(ROOT, "scripts", "install.sh")) as f:
            self.assertEqual(self.read(self.build(), "install.sh"), f.read())

    def test_the_copies_come_from_the_build_not_the_disk(self):
        # build() writes no page to disk: what is copied is what
        # on_page_markdown returned to MkDocs, in the one pass.
        self.assertIn("From [nothing](install.md)", self.read(self.build(), "quickstart.md"))

    def test_a_new_build_starts_empty(self):
        hooks.on_page_markdown("# Q\n", FakePage("q.md"), CONFIG, None)
        hooks.on_pre_build(CONFIG)
        self.assertEqual(hooks.BUILT, {})

    def test_a_long_first_sentence_is_cut(self):
        self.assertEqual(len(hooks.description("# T\n\n" + "word " * 100)), 198)


if __name__ == "__main__":
    unittest.main()
