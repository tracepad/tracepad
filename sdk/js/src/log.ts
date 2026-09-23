/**
 * The package's one logger (spec 032 #8).
 *
 * The tracing path never throws into application code; it says what happened
 * here instead. Node has no standard logger, so the default is `console.warn`
 * with a `tracepad:` prefix, and `init({ logger })` hands the lines to the
 * application's own — anything with a `warn(message)` method qualifies, which
 * `console` and every structured logger do.
 */

export interface Logger {
  warn(message: string): void;
  /** Where the lines nothing needs to act on go, if it has one; the default does not (stdout). */
  debug?(message: string): void;
}

let sink: Logger = { warn: (message) => console.warn(message) };

export function setLogger(logger: Logger): void {
  sink = logger;
}

export function warn(message: string): void {
  sink.warn(`tracepad: ${message}`);
}

export function debug(message: string): void {
  sink.debug?.(`tracepad: ${message}`);
}
