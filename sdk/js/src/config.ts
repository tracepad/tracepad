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
  const host = pick(options.host, 'TRACEPAD_HOST').replace(/\/+$/, '');
  const key = pick(options.key, 'TRACEPAD_API_KEY');
  const missing = [
    ['host', host],
    ['key', key],
  ]
    .filter(([, value]) => !value)
    .map(([name]) => name);
  if (missing.length > 0) {
    throw new TracepadConfigError(
      `tracepad: no ${missing.join(' and no ')}; pass them to init() or set TRACEPAD_HOST and TRACEPAD_API_KEY`,
    );
  }
  const config: { -readonly [K in keyof Config]: Config[K] } = { host, key };
  const environment = pick(options.environment, 'TRACEPAD_ENVIRONMENT');
  const release = pick(options.release, 'TRACEPAD_RELEASE');
  if (environment) config.environment = environment;
  if (release) config.release = release;
  return config;
}

/** The exporter's per-export timeout when nothing names one (spec 042 #3):
 * OpenTelemetry's ten seconds, its retries inside them, is long for a
 * request that flushes before it answers. */
export const EXPORT_TIMEOUT_MILLIS = 5000;

/** The option, then TRACEPAD_EXPORT_TIMEOUT (seconds), then five seconds — or
 * `undefined`, which leaves the exporter to OpenTelemetry's own variable when one is set. */
export function exportTimeout(millis: number | undefined): number | undefined {
  if (millis !== undefined) return millis;
  const raw = (process.env.TRACEPAD_EXPORT_TIMEOUT ?? '').trim();
  if (raw) {
    const seconds = Number(raw);
    if (Number.isFinite(seconds) && seconds > 0) return seconds * 1000;
    warn(`TRACEPAD_EXPORT_TIMEOUT=${JSON.stringify(raw)} is not a number of seconds; it is ignored`);
  }
  if (process.env.OTEL_EXPORTER_OTLP_TRACES_TIMEOUT || process.env.OTEL_EXPORTER_OTLP_TIMEOUT) return undefined;
  return EXPORT_TIMEOUT_MILLIS;
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
