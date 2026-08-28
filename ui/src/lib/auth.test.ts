import { beforeEach, describe, expect, it, vi } from 'vitest';

const goto = vi.fn();
const replaceState = vi.fn();
vi.mock('$app/navigation', () => ({ goto, replaceState }));

// The module keeps state, so each test gets a fresh copy of it.
async function freshAuth() {
	vi.resetModules();
	return (await import('./auth.svelte')).auth;
}

function at(url: string) {
	window.history.replaceState(null, '', url);
}

describe('the pre-authed URL', () => {
	beforeEach(() => {
		window.localStorage.clear();
		goto.mockClear();
		replaceState.mockClear();
		at('/');
	});

	it('signs in from the fragment and remembers the key', async () => {
		at('/#key=tp-sk-abc123');
		const auth = await freshAuth();

		auth.restore();

		expect(auth.key).toBe('tp-sk-abc123');
		expect(auth.authenticated).toBe(true);
		expect(window.localStorage.getItem('tracepad.key')).toBe('tp-sk-abc123');
	});

	it('takes the key back out of the URL once the router is up', async () => {
		at('/traces?status=error#key=tp-sk-abc123');
		const auth = await freshAuth();
		auth.restore();

		auth.stripFragment();

		// Replaced, never pushed: the link must not stay reachable through
		// the back button.
		expect(replaceState).toHaveBeenCalledWith('/traces?status=error', {});
	});

	it('leaves the URL alone when there is no fragment', async () => {
		at('/traces');
		const auth = await freshAuth();
		auth.restore();

		auth.stripFragment();

		expect(replaceState).not.toHaveBeenCalled();
	});

	it('beats a key left over from a previous visit', async () => {
		window.localStorage.setItem('tracepad.key', 'tp-sk-old');
		at('/#key=tp-sk-new');
		const auth = await freshAuth();

		auth.restore();

		expect(auth.key).toBe('tp-sk-new');
	});
});

describe('the stored key', () => {
	beforeEach(() => {
		window.localStorage.clear();
		goto.mockClear();
		replaceState.mockClear();
		at('/');
	});

	it('is restored when no fragment is present', async () => {
		window.localStorage.setItem('tracepad.key', 'tp-sk-stored');
		const auth = await freshAuth();

		auth.restore();

		expect(auth.key).toBe('tp-sk-stored');
	});

	it('is absent on a first visit', async () => {
		const auth = await freshAuth();

		auth.restore();

		expect(auth.authenticated).toBe(false);
	});

	it('is dropped and sent back to login when the server rejects it', async () => {
		window.localStorage.setItem('tracepad.key', 'tp-sk-revoked');
		const auth = await freshAuth();
		auth.restore();

		auth.reject();

		expect(auth.key).toBeNull();
		expect(window.localStorage.getItem('tracepad.key')).toBeNull();
		expect(goto).toHaveBeenCalledWith('/login', { replaceState: true });
	});

	it('does not bounce a visitor who never had a key', async () => {
		const auth = await freshAuth();
		auth.restore();

		auth.reject();

		expect(goto).not.toHaveBeenCalled();
	});

	it('refuses blank input from the login form', async () => {
		const auth = await freshAuth();

		expect(auth.adopt('   ')).toBe(false);
		expect(auth.authenticated).toBe(false);
	});

	it('trims what was pasted', async () => {
		const auth = await freshAuth();

		auth.adopt('  tp-sk-padded\n');

		expect(auth.key).toBe('tp-sk-padded');
	});
});

describe('where the login form sends somebody afterwards', () => {
	const at = (search: string) => new URL(`http://server/login${search}`);
	let returnTo: typeof import('./auth.svelte').returnTo;

	beforeEach(async () => {
		returnTo = (await import('./auth.svelte')).returnTo;
	});

	it('returns to the screen the guard interrupted', () => {
		expect(returnTo(at('?next=%2Ftraces%3Fstatus%3Derror'))).toBe('/traces?status=error');
	});

	it('falls back when nothing was asked for', () => {
		expect(returnTo(at(''))).toBe('/traces');
	});

	it('refuses anywhere but this origin', () => {
		for (const hostile of ['//elsewhere.example', 'https://elsewhere.example/x', '/\\elsewhere.example']) {
			expect(returnTo(at(`?next=${encodeURIComponent(hostile)}`))).toBe('/traces');
		}
	});

	it('keeps a path that merely looks odd', () => {
		// A backslash inside the path is not a scheme-relative URL.
		expect(returnTo(at('?next=%2Ftraces%2Fa%5Cb'))).toContain('/traces/');
	});
});
