"""The package's one logger.

The tracing path never raises into application code; it says what happened
here instead (spec 017 #9). The logger is left at its default level and with
no handler of its own, so an application's logging configuration owns it.
"""

from __future__ import annotations

import logging

logger = logging.getLogger("tracepad")
