/**
 * Prompts: fetched by label, cached for as long as the server says (spec 032 #7).
 *
 * A prompt is read per request and changes per deploy. The store already says
 * how long a label may be trusted (`Cache-Control: max-age=60`,
 * `docs/prompts.md`), so the cache honours that and nothing else; when the
 * store is away the last answer is served stale, because the reference
 * application must survive a restart of its own observability. With nothing
 * cached the call rejects: a fallback prompt baked into the code is a prompt
 * the trace cannot name.
 */

import { current } from './config.js';
import { TracepadError, TracepadHTTPError, maxAge, request } from './http.js';
import { warn } from './log.js';

export interface Message {
  role: string;
  content: string;
  [field: string]: unknown;
}

/** One version of a stored prompt. */
export class Prompt {
  readonly name: string;
  readonly version: number;
  readonly type: string;
  readonly text: string | undefined;
  readonly messages: Message[] | undefined;
  readonly labels: string[];
  readonly config: Record<string, unknown>;

  constructor(fields: {
    name: string;
    version: number;
    type: string;
    text?: string;
    messages?: Message[];
    labels?: string[];
    config?: Record<string, unknown>;
  }) {
    this.name = fields.name;
    this.version = fields.version;
    this.type = fields.type;
    this.text = fields.text;
    this.messages = fields.messages;
    this.labels = fields.labels ?? [];
    this.config = fields.config ?? {};
  }

  /**
   * Substitute `{name}` placeholders, in the text or in every message.
   *
   * A placeholder with no variable throws, the way Python's `str.format`
   * does: a prompt sent with a hole in it is a worse failure than one not
   * sent. Nothing else: a template language is a product, and what the
   * store stores is plain text.
   */
  compile(variables: Record<string, unknown> = {}): string | Message[] {
    if (this.messages !== undefined) {
      return this.messages.map((message) => ({
        ...message,
        content: fill(String(message.content ?? ''), variables),
      }));
    }
    return fill(this.text ?? '', variables);
  }
}

function fill(template: string, variables: Record<string, unknown>): string {
  return template.replace(/\{([^{}]+)\}/g, (_match, name: string) => {
    if (!Object.hasOwn(variables, name)) {
      throw new TracepadError(`tracepad: prompt placeholder {${name}} has no variable`);
    }
    return String(variables[name]);
  });
}

interface Entry {
  prompt: Prompt;
  expiresAt: number;
}

const cache = new Map<string, Entry>();

export interface PromptOptions {
  label?: string;
  version?: number;
}

/** Fetch a prompt by label, by version, or the latest of them. */
export async function prompt(name: string, options: PromptOptions = {}): Promise<Prompt> {
  const { label, version } = options;
  const key = JSON.stringify([name, label ?? null, version ?? null]);
  const cached = cache.get(key);
  if (cached !== undefined && cached.expiresAt > performance.now()) return cached.prompt;

  let answer;
  try {
    answer = await request(current(), 'GET', `/api/v1/prompts/${encodeURIComponent(name)}`, {
      params: { label, version },
    });
  } catch (error) {
    if (cached === undefined || isClientError(error)) throw error;
    // The store restarting must not take the application down; the label
    // it moved meanwhile is late by at most the window it published.
    warn(`serving prompt ${JSON.stringify(name)} from a stale cache: ${(error as Error).message}`);
    return cached.prompt;
  }
  const fetched = read(answer.body);
  cache.set(key, { prompt: fetched, expiresAt: performance.now() + maxAge(answer.headers) * 1000 });
  return fetched;
}

/**
 * A 4xx is about the request, not about the server being away: a moved
 * label, a deleted prompt or a wrong key must be thrown, not papered over.
 */
function isClientError(error: unknown): boolean {
  return error instanceof TracepadHTTPError && error.status >= 400 && error.status < 500;
}

function read(body: unknown): Prompt {
  if (body === null || typeof body !== 'object' || Array.isArray(body)) {
    throw new TracepadError(`tracepad: unexpected prompt answer: ${JSON.stringify(body)}`);
  }
  const answer = body as Record<string, unknown>;
  const stored = answer.prompt;
  const fields: ConstructorParameters<typeof Prompt>[0] = {
    name: typeof answer.name === 'string' ? answer.name : '',
    version: typeof answer.version === 'number' ? answer.version : 0,
    type: typeof answer.type === 'string' ? answer.type : '',
    labels: Array.isArray(answer.labels) ? answer.labels.map(String) : [],
    config: answer.config !== null && typeof answer.config === 'object' ? { ...answer.config } : {},
  };
  if (typeof stored === 'string') fields.text = stored;
  if (Array.isArray(stored)) fields.messages = stored as Message[];
  return new Prompt(fields);
}

/** Empty the cache. For tests. */
export function forget(): void {
  cache.clear();
}
