/**
 * What `updateTrace` has said on a span so far (spec 032 #24).
 *
 * OpenTelemetry keeps one value per attribute, so a second call that wrote
 * `tracepad.trace.tags` or `tracepad.trace.metadata` again replaced the first
 * call's. The package remembers what it has written on each span, and merges
 * each call into that: tags in the order first seen, metadata by top-level key
 * with the later value winning, both written whole — the wire is one JSON array
 * and one JSON object, as it always was.
 *
 * Only what this package wrote is merged: a span's attributes are not part of
 * the OpenTelemetry API, so tags another writer set on the same span are
 * replaced by the first call.
 */
import * as attrs from './attributes.js';
import { warn } from './log.js';

/** The server's own bounds (spec 043 #14, spec 002 #32): beyond them it drops what it was sent. */
export const MAX_TAGS = 50;
export const MAX_KEYS = 512;
export const MAX_BYTES = 1 << 20;

interface State {
  tags: string[];
  metadata: Record<string, unknown>;
}

const states = new WeakMap<object, State>();
let warned = false;

function warnOnce(message: string): void {
  if (!warned) warn(message);
  warned = true;
}

/** Merge a call into the span's state; the attributes to write, whole. */
export function merge(span: object, tags?: Iterable<string>, metadata?: unknown): Record<string, string> {
  let state = states.get(span);
  if (state === undefined) states.set(span, (state = { tags: [], metadata: {} }));
  const written: Record<string, string> = {};
  if (tags !== undefined) {
    for (const tag of tags) {
      if (!state.tags.includes(tag) && state.tags.length < MAX_TAGS) state.tags.push(tag);
    }
    written[attrs.TRACE_TAGS] = attrs.dumps(state.tags);
  }
  if (metadata !== undefined) {
    const encoded = mergeMetadata(state.metadata, metadata);
    if (encoded !== undefined) written[attrs.TRACE_METADATA] = encoded;
  }
  return written;
}

function mergeMetadata(held: Record<string, unknown>, added: unknown): string | undefined {
  if (typeof added !== 'object' || added === null || Array.isArray(added)) {
    warnOnce('updateTrace({ metadata }) takes an object; this one was ignored');
    return undefined;
  }
  let candidate: Record<string, unknown> = { ...held };
  const entries = Object.entries(added);
  for (const [key, value] of entries) {
    if (key in held || Object.keys(candidate).length < MAX_KEYS) candidate[key] = value;
    else warnOnce(`trace metadata is bounded at ${MAX_KEYS} keys; the new keys past it were dropped`);
  }
  let encoded = attrs.dumps(candidate);
  if (Buffer.byteLength(encoded) > MAX_BYTES) {
    candidate = { ...held };
    for (const [key, value] of entries) {
      const trial = { ...candidate, [key]: value };
      if ((key in held || Object.keys(candidate).length < MAX_KEYS) && Buffer.byteLength(attrs.dumps(trial)) <= MAX_BYTES) {
        candidate = trial;
      } else warnOnce(`trace metadata is bounded at ${MAX_BYTES} bytes; the keys past it were dropped`);
    }
    encoded = attrs.dumps(candidate);
  }
  for (const key of Object.keys(held)) delete held[key];
  Object.assign(held, candidate);
  return encoded;
}
