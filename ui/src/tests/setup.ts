import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/svelte';
import { afterAll, afterEach } from 'vitest';

// Components mounted by one test must not still be in the document when the
// next one queries it.
afterEach(cleanup);

// Unmounting a bits-ui overlay — a Dialog, a Popover, anything that locks the
// body — does not restore the body style there and then: `BodyScrollLock`
// schedules the reset 24ms later, so that an overlay destroyed and recreated in
// the same tick keeps its lock (`bits-ui/internal/body-scroll-lock.svelte.js`).
// That timer is Node's own, not the document's. Vitest's jsdom environment
// leaves `setTimeout` alone when it puts the window's properties on the global
// object, and `window` *is* that object, so the `window.close()` ending the file
// cancels jsdom's timers and never this one. Left to fire on its own it reads
// `document` after the file that owned it is gone, and the `ReferenceError`
// lands outside every test, where vitest counts it as an unhandled error and
// fails a run in which each test passed. So spend the 24ms here, while the
// document is still standing. This is not a slow test being given room: remove
// the wait and the suite goes red once every few runs, on whichever overlay test
// happened to unmount last.
afterAll(() => new Promise((resolve) => setTimeout(resolve, 50)));
