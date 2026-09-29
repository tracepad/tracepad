# tracepad

The Node package for [Tracepad](https://github.com/tracepad/tracepad), a
lightweight, self-hosted, OTLP-native store and viewer for LLM traces. A thin
layer of ergonomics over the OpenTelemetry SDK: it owns no transport, no
batching, no retry and no context propagation, and it wraps no provider
client.

```sh
npm install tracepad @opentelemetry/api
```

```ts
import * as tracepad from 'tracepad';

tracepad.init(); // TRACEPAD_URL and TRACEPAD_API_KEY from the environment

const answer = tracepad.observe(async (question: string) => {
  tracepad.updateTrace({ userId: 'u-42', tags: ['support'] });
  const reply = await tracepad.generation('chat', { model: 'gpt-4o-mini', input: question }, async (call) => {
    const response = await client.chat.completions.create({ model: 'gpt-4o-mini', messages: [{ role: 'user', content: question }] });
    call.end(response); // model, usage, and the cost as charged
    return response.choices[0].message.content;
  });
  tracepad.score('helpful', 1); // against the trace in flight
  return reply;
});
```

Node 22 or newer; ESM and CommonJS, with types. Built by `tsup`.

The whole surface — `init`, `observe`, `span`, `event`, `generation` with
streams, `update`, `updateTrace`, `score`, `prompt`, `flush` — is documented in
[docs/sdk-js.md](https://github.com/tracepad/tracepad/blob/main/docs/sdk-js.md).
