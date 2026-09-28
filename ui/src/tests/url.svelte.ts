/**
 * A `page.url` that moves the way SvelteKit's does — a new `URL` on every
 * navigation, read reactively — for a test that needs a component to see the
 * screen change under it. Mock `$app/state` with a getter that reads `here.url`.
 */
export const here = $state({ url: new URL('http://tracepad.test/') });
