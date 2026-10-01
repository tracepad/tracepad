"""The hook that points docs/'s links out of itself at GitHub (spec 050 #5).

Run by `make docs-site-test`, through the same locked environment as the
build. The repository root is the real one, so `README.md` and `scripts/smoke`
are the files and the directory the hook looks for.
"""

import os
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
            rewrite("[readme]: ../README.md#getting-it \"the README\"\n"),
            f"[readme]: {GITHUB}/blob/main/README.md#getting-it \"the README\"\n",
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


if __name__ == "__main__":
    unittest.main()
