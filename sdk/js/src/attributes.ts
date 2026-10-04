/**
 * The vocabulary the package writes (spec 032 #3): exactly spec 017 #3's.
 *
 * The OTel GenAI semantic conventions where a name exists, `tracepad.*` where
 * none does, and `langfuse.*` never. The `tracepad.*` half of this file is
 * the mapper's table in `internal/mapping/rules.go`, and `docs/ingest.md`
 * prints both; the Python package writes the same keys, and the golden
 * fixture of Decision 12 is what proves the two agree.
 */

// GenAI semantic conventions.
export const REQUEST_MODEL = 'gen_ai.request.model';
export const RESPONSE_MODEL = 'gen_ai.response.model';
export const REQUEST_PREFIX = 'gen_ai.request.';
export const USAGE_PREFIX = 'gen_ai.usage.';
export const USAGE_COST = 'gen_ai.usage.cost';
export const INPUT = 'gen_ai.input.messages';
export const OUTPUT = 'gen_ai.output.messages';

// Trace-level names the conventions do have.
export const USER_ID = 'user.id';
export const SESSION_ID = 'session.id';
export const ENVIRONMENT = 'deployment.environment.name';
export const SERVICE_NAME = 'service.name';
export const SERVICE_VERSION = 'service.version';

// Tracepad's own, for the facts the conventions have no word for.
export const TRACE_NAME = 'tracepad.trace.name';
export const TRACE_TAGS = 'tracepad.trace.tags';
export const TRACE_METADATA = 'tracepad.trace.metadata';
export const TRACE_VERSION = 'tracepad.trace.version';
export const OBSERVATION_TYPE = 'tracepad.observation.type';
export const OBSERVATION_LEVEL = 'tracepad.observation.level';
export const OBSERVATION_STATUS_MESSAGE = 'tracepad.observation.status_message';
export const OBSERVATION_METADATA = 'tracepad.observation.metadata';
export const COMPLETION_START_TIME = 'tracepad.observation.completion_start_time';
export const PROMPT_NAME = 'tracepad.prompt.name';
export const PROMPT_VERSION = 'tracepad.prompt.version';

// The run link (spec 014 #2): stamped on every span of an eval by the
// processor of the harness, and not a dialect of its own.
export const RUN_ID = 'tracepad.run_id';
export const ITEM_ID = 'tracepad.item_id';

/** The ten kinds an observation may be (`docs/ingest.md`). */
export const OBSERVATION_TYPES = [
  'span',
  'generation',
  'event',
  'agent',
  'tool',
  'chain',
  'retriever',
  'guardrail',
  'evaluator',
  'embedding',
] as const;

export type ObservationType = (typeof OBSERVATION_TYPES)[number];

/** The four levels schema 0002 allows. */
export type Level = 'DEBUG' | 'DEFAULT' | 'WARNING' | 'ERROR';

/**
 * Render a payload for an attribute.
 *
 * A string is sent as it is — a plain-text prompt is a plain-text payload,
 * not a JSON string of one (spec 015 #12) — and everything else is JSON
 * under a replacer that never throws: a `bigint` becomes a number, an
 * `Error` its name and message, a function or a symbol its `String()`. A
 * value the encoder still refuses — a cycle — is its `String()` whole,
 * because the alternative is a wrapper that breaks the function it wraps.
 */
export function dumps(value: unknown): string {
  if (typeof value === 'string') return value;
  try {
    const encoded = JSON.stringify(value, replacer);
    return encoded === undefined ? String(value) : encoded;
  } catch {
    return String(value);
  }
}

/** A value as JSON, a string too: a part of a larger document, which `dumps` is not. Never throws. */
export function jsonText(value: unknown): string {
  try {
    return JSON.stringify(value, replacer) ?? JSON.stringify(String(value));
  } catch {
    return JSON.stringify(String(value));
  }
}

function replacer(this: unknown, _key: string, value: unknown): unknown {
  if (typeof value === 'bigint') return Number(value);
  if (value instanceof Error) return { name: value.name, message: value.message };
  if (typeof value === 'function' || typeof value === 'symbol') return String(value);
  return value;
}

/** Render a model parameter or a metadata entry, keeping the types OTLP has
 * of its own. A value that encodes as a JSON string — a `Date` — is that
 * string, not the string with its quotes (found in review of PR #83). */
export function scalar(value: unknown): string | number | boolean {
  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
    return value;
  }
  if (typeof value === 'bigint') return Number(value); // as `dumps` writes one
  const encoded = dumps(value);
  try {
    return encoded.startsWith('"') ? (JSON.parse(encoded) as string) : encoded;
  } catch {
    return encoded; // `String()` of a value the encoder refused, and not JSON
  }
}

/** More keys than this in one write are written whole, as one attribute: a
 * span holds 128 attributes by default (OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT), and
 * when it is full the ones dropped are the step's own (found in review of PR #83). */
export const MAX_METADATA_KEYS = 32;

/**
 * Observation metadata as attributes: one per top-level key, so that a later
 * write adds keys rather than replacing the lot (spec 042 #5), a key without
 * a value writing nothing. An object that is not a plain record — a class
 * with `toJSON`, a `Map` — is taken as it encodes. Written whole under the
 * one key instead when it encodes as no object, has more than
 * `MAX_METADATA_KEYS` keys, or a key the per-key form cannot name — an empty one.
 */
export function metadata(value: unknown): Record<string, string | number | boolean> {
  const plain = isRecord(value) && [Object.prototype, null].includes(Object.getPrototypeOf(value));
  const entries = plain ? value : decoded(value);
  const keys = isRecord(entries) ? Object.keys(entries) : [];
  if (!isRecord(entries) || keys.length > MAX_METADATA_KEYS || keys.includes('')) {
    return { [OBSERVATION_METADATA]: dumps(value) };
  }
  const out: Record<string, string | number | boolean> = {};
  for (const [key, entry] of Object.entries(entries)) {
    if (entry != null) out[`${OBSERVATION_METADATA}.${key}`] = scalar(entry);
  }
  return out;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function decoded(value: unknown): unknown {
  try {
    return JSON.parse(dumps(value)) as unknown;
  } catch {
    return undefined;
  }
}

/** An instant as the mapper reads it (`docs/ingest.md`, time to first token). */
export function rfc3339(milliseconds: number): string {
  return new Date(milliseconds).toISOString();
}
