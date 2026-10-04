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
 * What is kept is JSON text, taken when the call is made: a value the
 * application changes afterwards is not changed in the trace, and a document is
 * put together by joining what is already encoded. A `Map` keeps the keys, so
 * `__proto__` is a key like any other.
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
  tags: Set<string>; // the JSON text of each tag
  metadata: Map<string, { text: string; bytes: number }>; // by the JSON text of the key
  size: number; // bytes of the object these make: braces, entries, commas
}

const states = new WeakMap<object, State>();
const warned = new Set<string>();
const encoder = new TextEncoder();
const bytesOf = (text: string): number => encoder.encode(text).length;

/** Forget what was warned about (`testing.reset`, spec 040). The states go with their spans. */
export function reset(): void {
  warned.clear();
}

/** Once per process for each kind: a bound that bites is said, not repeated on every call. */
function warnOnce(kind: string, message: string): void {
  if (!warned.has(kind)) warn(message);
  warned.add(kind);
}

/** Merge a call into the span's state; the attributes to write, whole. */
export function merge(span: object, tags?: Iterable<string>, metadata?: unknown): Record<string, string> {
  let state = states.get(span);
  if (state === undefined) states.set(span, (state = { tags: new Set(), metadata: new Map(), size: 2 }));
  const written: Record<string, string> = {};
  if (tags !== undefined) {
    for (const tag of tags) {
      const text = attrs.jsonText(tag);
      if (state.tags.has(text)) continue;
      if (state.tags.size < MAX_TAGS) state.tags.add(text);
      else warnOnce('tags', `trace tags are bounded at ${MAX_TAGS}; the tags past them were dropped`);
    }
    written[attrs.TRACE_TAGS] = `[${[...state.tags].join(',')}]`;
  }
  if (metadata !== undefined && mergeMetadata(state, metadata)) {
    written[attrs.TRACE_METADATA] = `{${[...state.metadata].map(([key, { text }]) => `${key}:${text}`).join(',')}}`;
  }
  return written;
}

function mergeMetadata(state: State, added: unknown): boolean {
  if (typeof added !== 'object' || added === null || Array.isArray(added)) {
    warnOnce('shape', 'updateTrace({ metadata }) takes an object; this one was ignored');
    return false;
  }
  for (const [key, value] of Object.entries(added)) {
    const name = JSON.stringify(key);
    const text = attrs.jsonText(value);
    const bytes = bytesOf(text);
    const held = state.metadata.get(name);
    let total: number;
    if (held !== undefined) total = state.size - held.bytes + bytes;
    else if (state.metadata.size >= MAX_KEYS) {
      warnOnce('keys', `trace metadata is bounded at ${MAX_KEYS} keys; the new keys past it were dropped`);
      continue;
    } else total = state.size + bytesOf(name) + 1 + bytes + (state.metadata.size > 0 ? 1 : 0);
    if (total > MAX_BYTES) {
      warnOnce('bytes', `trace metadata is bounded at ${MAX_BYTES} bytes; the keys past it were dropped`);
      continue;
    }
    state.metadata.set(name, { text, bytes });
    state.size = total;
  }
  return true;
}
