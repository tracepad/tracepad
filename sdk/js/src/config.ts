/**
 * Configuration: the arguments of `init`, then the environment (spec 032 #8).
 *
 * Standard OpenTelemetry variables — `OTEL_SERVICE_NAME`,
 * `OTEL_RESOURCE_ATTRIBUTES`, the exporter's and the batch processor's own —
 * are honoured by the OTel SDK as they are. The package neither reads them
 * nor sets them: the SDK already implements them, with its own defaults, and
 * a second implementation would be a second set of answers.
 */

import { TracepadConfigError } from './http.js';
import { warn } from './log.js';

export interface Config {
  readonly host: string;
  readonly key: string;
  readonly environment?: string;
  readonly release?: string;
}

export interface ConfigOptions {
  host?: string;
  key?: string;
  environment?: string;
  release?: string;
}

/** Build a Config, throwing when the two required values are nowhere. */
export function resolve(options: ConfigOptions = {}): Config {
  const [picked, deprecated] = pickHost(options.host);
  const host = trimTrailingSlashes(picked);
  const key = pick(options.key, 'TRACEPAD_API_KEY');
  const missing = [
    ['host', host],
    ['key', key],
  ]
    .filter(([, value]) => !value)
    .map(([name]) => name);
  if (missing.length > 0) {
    throw new TracepadConfigError(
      `tracepad: no ${missing.join(' and no ')}; pass them to init() or set TRACEPAD_URL and TRACEPAD_API_KEY`,
    );
  }
  if (deprecated && !hostWarned) {
    hostWarned = true;
    warn('TRACEPAD_HOST is deprecated; set TRACEPAD_URL, which the CLI and the server read too');
  }
  const config: { -readonly [K in keyof Config]: Config[K] } = { host, key };
  const environment = pick(options.environment, 'TRACEPAD_ENVIRONMENT');
  const release = pick(options.release, 'TRACEPAD_RELEASE');
  if (environment) config.environment = environment;
  if (release) config.release = release;
  // Read, never listed: out of `console.log`, `util.inspect` and
  // `JSON.stringify`, which are what an error tracker renders a value with
  // (spec 032 #17).
  Object.defineProperty(config, 'key', { enumerable: false });
  return config;
}

/** The exporter's per-export timeout when nothing names one (spec 042 #3):
 * OpenTelemetry's ten seconds, its retries inside them, is long for a
 * request that flushes before it answers. */
export const EXPORT_TIMEOUT_MILLIS = 5000;

/** The option, then TRACEPAD_EXPORT_TIMEOUT (seconds), then five seconds — or
 * `undefined`, which leaves the exporter to OpenTelemetry's own variable when one is set. */
export function exportTimeout(millis: number | undefined): number | undefined {
  // Checked here rather than left to the exporter, which throws out of `init`
  // on a timeout it refuses — and past 2^31 - 1 a timer fires at once, so
  // every export would time out (found in review of PR #83).
  const valid = (ms: number) => Number.isFinite(ms) && ms > 0 && ms <= 2 ** 31 - 1;
  if (millis !== undefined) {
    if (valid(millis)) return millis;
    warn(`exportTimeoutMillis=${String(millis)} is not a positive number of milliseconds; it is ignored`);
  }
  const raw = (process.env.TRACEPAD_EXPORT_TIMEOUT ?? '').trim();
  if (raw) {
    if (valid(Number(raw) * 1000)) return Number(raw) * 1000;
    warn(`TRACEPAD_EXPORT_TIMEOUT=${JSON.stringify(raw)} is not a number of seconds; it is ignored`);
  }
  if (process.env.OTEL_EXPORTER_OTLP_TRACES_TIMEOUT || process.env.OTEL_EXPORTER_OTLP_TIMEOUT) return undefined;
  return EXPORT_TIMEOUT_MILLIS;
}

let hostWarned = false;

/** Forget that the deprecated-host warning was given; `tracepad/testing`'s
 * `reset` calls it, and the package's index does not export it. */
export function rearmHostWarning(): void {
  hostWarned = false;
}

/** The option, then TRACEPAD_URL — the name the CLI and the server read too
 * (spec 032 #23) — then TRACEPAD_HOST, this package's first name for it: it
 * still works. The flag says it was the one used; `resolve` warns, once, when
 * the configuration is complete. */
function pickHost(argument: string | undefined): [host: string, deprecated: boolean] {
  if (argument != null) return [argument.trim(), false];
  const url = (process.env.TRACEPAD_URL ?? '').trim();
  if (url) return [url, false];
  const legacy = (process.env.TRACEPAD_HOST ?? '').trim();
  return [legacy, legacy !== ''];
}

/** Without the slashes at the end. A loop, not `/\/+$/`: a regex engine
 * retries that one at every slash of a long run that does not end the
 * string, which is quadratic in the run. */
function trimTrailingSlashes(host: string): string {
  let end = host.length;
  while (end > 0 && host.charCodeAt(end - 1) === 0x2f) end--;
  return host.slice(0, end);
}

function pick(argument: string | undefined, variable: string): string {
  return (argument ?? process.env[variable] ?? '').trim();
}

let active: Config | undefined;

export function adopt(config: Config): void {
  active = config;
}

/**
 * The configuration the REST calls use.
 *
 * `init` is the ordinary way to set it, and a script that only fetches a
 * prompt should not have to call it: with no `init`, the environment is read
 * on the first call and the same `TracepadConfigError` is thrown when it
 * says nothing.
 */
export function current(): Config {
  if (active === undefined) active = resolve();
  return active;
}

/** Drop the process-wide configuration. For tests. */
export function forget(): void {
  active = undefined;
}
