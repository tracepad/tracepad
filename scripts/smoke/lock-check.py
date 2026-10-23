"""Fail when requirements.lock does not hold a pin requirements.txt names.

    python3 scripts/smoke/lock-check.py

The lock is compiled from requirements.txt and sdk/python's dependencies with
the command in requirements.txt's header; an edit to requirements.txt without
a new lock leaves a pin here that the lock lacks. Reads files only, so it does
not go red when the index changes: recompiling and diffing would, the day a
pinned version gains a wheel and with it a hash. That sdk/python's own
dependencies are in the lock is what `pip check` after the install proves.

Standard library only: it runs before the environment it checks exists.
"""

import pathlib
import re
import sys

here = pathlib.Path(__file__).resolve().parent


def pins(path, continued):
    """name -> version for every `name==version` line, names normalized."""
    found = {}
    for line in path.read_text().splitlines():
        if not line or line[0] in "# \t-":
            continue
        req = line.split(";")[0].rstrip(" \\") if continued else line.split(";")[0]
        m = re.fullmatch(r"\s*([A-Za-z0-9][A-Za-z0-9._-]*)\s*==\s*(\S+)\s*", req)
        if m:
            found[re.sub(r"[-_.]+", "-", m.group(1)).lower()] = m.group(2)
    return found


wanted = pins(here / "requirements.txt", continued=False)
held = pins(here / "requirements.lock", continued=True)
missing = [f"{n}=={v}" for n, v in wanted.items() if held.get(n) != v]
if missing:
    for pin in missing:
        print(f"{pin} is not in scripts/smoke/requirements.lock", file=sys.stderr)
    print("regenerate it with the command in scripts/smoke/requirements.txt", file=sys.stderr)
    sys.exit(1)
