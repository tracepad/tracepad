/**
 * Reading an OpenAI-compatible response, whole (spec 017 #5) or streamed (spec 031 #7).
 *
 * One reader, one shape. OpenRouter puts the charge the provider actually
 * made in `usage.cost`, and OpenAI's envelope is what every proxy and most
 * providers speak, so this carries the common case; every other provider is
 * carried by the explicit fields of `Generation.end`, which win over anything
 * read here. The cost is never estimated — what this cannot read, it does not
 * send, so the store shows *no data* rather than `$0`.
 */

export interface Usage {
  [name: string]: number;
}

export interface Fields {
  model?: string;
  usage?: Usage;
  cost?: number;
  output?: unknown;
}

/** The fields of an OpenAI-compatible response, by our own names. */
export function readResponse(response: unknown): Fields {
  const fields: Fields = {};
  const model = get(response, 'model');
  if (typeof model === 'string' && model) fields.model = model;

  const usage = get(response, 'usage');
  if (usage !== undefined && usage !== null) {
    const counts: Usage = {};
    count(counts, 'input_tokens', get(usage, 'prompt_tokens'));
    count(counts, 'output_tokens', get(usage, 'completion_tokens'));
    count(counts, 'cache_read_input_tokens', get(get(usage, 'prompt_tokens_details'), 'cached_tokens'));
    count(counts, 'reasoning_tokens', get(get(usage, 'completion_tokens_details'), 'reasoning_tokens'));
    if (Object.keys(counts).length > 0) fields.usage = counts;
    const cost = get(usage, 'cost');
    if (isNumber(cost)) fields.cost = cost;
  }

  const content = get(get(firstChoice(response), 'message'), 'content');
  if (content !== undefined && content !== null) fields.output = content;
  return fields;
}

function firstChoice(response: unknown): unknown {
  const choices = get(response, 'choices');
  return Array.isArray(choices) ? choices[0] : undefined;
}

function get(object: unknown, name: string): unknown {
  if (object === null || typeof object !== 'object') return undefined;
  return (object as Record<string, unknown>)[name];
}

function count(counts: Usage, name: string, value: unknown): void {
  if (isNumber(value)) counts[name] = value;
}

function isNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value);
}

/**
 * What a streamed answer has said so far (spec 031 #7).
 *
 * A stream is the same answer cut into chunks: the model is named on each of
 * them, the content arrives as deltas, and the usage — with the cost, on
 * OpenRouter — rides on the last one, when the caller asked for it at all.
 * `take` reads one chunk; `response` folds what was taken into the one
 * envelope `readResponse` already reads, so a stream is not a second table
 * of fields.
 */
export class Stream {
  model: string | undefined;
  usage: unknown;
  readonly parts: string[] = [];

  /** Read one chunk. True when it carried content, which is a token. */
  take(chunk: unknown): boolean {
    const model = get(chunk, 'model');
    if (typeof model === 'string' && model) this.model = model;
    const usage = get(chunk, 'usage');
    if (usage !== undefined && usage !== null) this.usage = usage;
    const content = get(get(firstChoice(chunk), 'delta'), 'content');
    if (typeof content === 'string') {
      this.parts.push(content);
      return content.length > 0;
    }
    return false;
  }

  /** The chunks as one answer, in the shape of a non-streamed one. */
  response(): Record<string, unknown> {
    const response: Record<string, unknown> = {};
    if (this.model !== undefined) response.model = this.model;
    if (this.usage !== undefined) response.usage = this.usage;
    if (this.parts.length > 0) response.choices = [{ message: { content: this.parts.join('') } }];
    return response;
  }
}
