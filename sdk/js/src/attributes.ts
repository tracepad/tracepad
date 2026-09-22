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

function replacer(this: unknown, _key: string, value: unknown): unknown {
  if (typeof value === 'bigint') return Number(value);
  if (value instanceof Error) return { name: value.name, message: value.message };
  if (typeof value === 'function' || typeof value === 'symbol') return String(value);
  return value;
}

/** Render a model parameter, keeping the types OTLP has of its own. */
export function scalar(value: unknown): string | number | boolean {
  if (typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean') {
    return value;
  }
  return dumps(value);
}

/** An instant as the mapper reads it (`docs/ingest.md`, time to first token). */
export function rfc3339(milliseconds: number): string {
  return new Date(milliseconds).toISOString();
}
