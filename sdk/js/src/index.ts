/**
 * Tracepad — the ergonomics of tracing an LLM application, over the OTel SDK.
 *
 *     import * as tracepad from 'tracepad';
 *
 *     tracepad.init(); // TRACEPAD_HOST / TRACEPAD_API_KEY
 *
 *     const answer = tracepad.observe(async (question: string) => {
 *       return tracepad.generation('chat', { model: 'gpt-4o-mini' }, async (call) => {
 *         const response = await client.chat.completions.create(...);
 *         call.end(response);
 *         return response.choices[0].message.content;
 *       });
 *     });
 *
 * Everything this package does is reachable with the OpenTelemetry SDK and
 * `fetch` — see `docs/sdk-js.md` and `docs/ingest.md`. It owns no transport,
 * no batching, no retry and no context propagation; those are the OTel
 * SDK's, and it wraps no provider client.
 */

export type { Level, ObservationType } from './attributes.js';
export { Dataset, dataset, type Item, type RunOptions } from './datasets.js';
export type { Usage } from './generation.js';
export {
  Attempt,
  Run,
  compare,
  itemId,
  scoreConfigs,
  type CloseOptions,
  type RunItemsOptions,
  type ScoreConfig,
} from './harness.js';
export { TracepadConfigError, TracepadError, TracepadHTTPError, VERSION } from './http.js';
export type { Logger } from './log.js';
export { Prompt, prompt, type Message, type PromptOptions } from './prompts.js';
export { score, type ScoreFields } from './scores.js';
export { deleteTrace, deleteTraces, type DeleteTracesOptions, type TraceFilter } from './traces.js';
export {
  Generation,
  Observation,
  event,
  flush,
  generation,
  init,
  observe,
  span,
  spanProcessor,
  update,
  updateTrace,
  type EndFields,
  type FlushOptions,
  type GenerationOptions,
  type InitOptions,
  type ObservationOptions,
  type ObserveOptions,
  type SpanOptions,
  type TraceFields,
  type UpdateFields,
} from './tracing.js';
