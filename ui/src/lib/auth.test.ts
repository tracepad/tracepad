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

const me = (account: Partial<{ name: string; owner: boolean }> = {}) => ({
	account: {
		id: 'acc1',
		email: 'her@example.com',
		name: account.name ?? '',
		owner: account.owner ?? false
	},
	projects: [{ id: 'p1', name: 'checkout', role: 'viewer' as const }]
});

beforeEach(() => {
	window.localStorage.clear();
	goto.mockClear();
	replaceState.mockClear();
	at('/');
});

describe('who is signed in', () => {
	it('is nobody until `me` has answered', async () => {
		const auth = await freshAuth();

		expect(auth.signedIn).toBe(false);
		expect(auth.account).toBeNull();
		expect(auth.projects).toEqual([]);
	});

	it('is the display name when there is one, and the email otherwise', async () => {
		const auth = await freshAuth();

		auth.adopt(me());
		expect(auth.displayName).toBe('her@example.com');

		auth.adopt(me({ name: 'Ada' }));
		expect(auth.displayName).toBe('Ada');
	});

	it('says whether this account runs the server', async () => {
		const auth = await freshAuth();

		auth.adopt(me({ owner: true }));

		expect(auth.owner).toBe(true);
	});

	// Nothing about the credential is kept: the session is an HttpOnly cookie
	// the browser holds, which is the whole point of Decision 4.
	it('keeps nothing in localStorage', async () => {
		const auth = await freshAuth();

		auth.adopt(me());

		expect(window.localStorage.length).toBe(0);
	});
});

describe('a session that has ended', () => {
	it('sends the reader to the login form with where they were', async () => {
		at('/traces?status=error');
		const auth = await freshAuth();
		auth.adopt(me());

		auth.reject();

		expect(auth.signedIn).toBe(false);
		expect(goto).toHaveBeenCalledWith('/login?next=%2Ftraces%3Fstatus%3Derror', {
			replaceState: true
		});
	});

	it('does not bounce somebody who is already on the login form', async () => {
		at('/login');
		const auth = await freshAuth();

		auth.reject();

		expect(goto).not.toHaveBeenCalled();
	});
});

describe('a link that carries a token', () => {
	it('reads the token out of the fragment', async () => {
		at('/setup#token=abc123');
		const auth = await freshAuth();

		expect(auth.tokenFromFragment()).toBe('abc123');
	});

	it('has none when the fragment carries something else', async () => {
		at('/invite#key=tp-sk-old');
		const auth = await freshAuth();

		expect(auth.tokenFromFragment()).toBeNull();
	});

	it('takes it back out of the URL', async () => {
		at('/invite?from=chat#token=abc123');
		const auth = await freshAuth();

		auth.stripFragment();

		// Replaced, never pushed: the link must not stay reachable through the
		// back button (spec 006 #8). The path and the query are untouched.
		expect(window.location.pathname + window.location.search).toBe('/invite?from=chat');
		expect(window.location.hash).toBe('');
	});

	it('leaves the URL alone when there is no fragment', async () => {
		at('/invite?from=chat');
		const auth = await freshAuth();

		auth.stripFragment();

		expect(window.location.pathname + window.location.search).toBe('/invite?from=chat');
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
		for (const hostile of [
			'//elsewhere.example',
			'https://elsewhere.example/x',
			'/\\elsewhere.example'
		]) {
			expect(returnTo(at(`?next=${encodeURIComponent(hostile)}`))).toBe('/traces');
		}
	});

	// Coming back to the login form is a loop, and coming back to an
	// invitation is a token that has just been spent.
	it('refuses the screens outside the shell', () => {
		for (const outside of ['/login', '/setup', '/invite']) {
			expect(returnTo(at(`?next=${encodeURIComponent(outside)}`))).toBe('/traces');
		}
	});

	it('keeps a path that merely looks odd', () => {
		// A backslash inside the path is not a scheme-relative URL.
		expect(returnTo(at('?next=%2Ftraces%2Fa%5Cb'))).toContain('/traces/');
	});
});
