import { beforeEach, describe, expect, it, vi } from 'vitest';

// The one route guard (spec 028, Application contract) and its three outcomes:
// a server with no owner sends everybody to `/setup`, nobody signed in goes to
// `/login` with where they were going, and anybody signed in gets the shell.

const bootstrap = vi.fn();
const needsSetup = vi.fn();

vi.mock('$app/navigation', () => ({ goto: vi.fn(), replaceState: vi.fn() }));
vi.mock('$lib/session', () => ({ bootstrap: () => bootstrap(), needsSetup: () => needsSetup() }));
vi.mock('$lib/theme.svelte', () => ({ theme: { restore: vi.fn() } }));

/**
 * A guard that has not run yet, over a session that says what the arguments
 * say. The module remembers that it has started, so each case needs its own
 * copy of it.
 */
async function guard(signedIn: boolean, setupRequired: boolean) {
	vi.resetModules();
	bootstrap.mockResolvedValue({ setupRequired });
	// The server goes on saying whatever it said at boot unless a test moves
	// it, which is what the "an owner appeared" case does.
	needsSetup.mockResolvedValue(setupRequired);
	const { auth } = await import('$lib/auth.svelte');
	if (signedIn) {
		auth.adopt({
			account: { id: 'acc1', email: 'her@example.com', name: '', owner: false },
			projects: []
		});
	}
	const { load } = await import('./+layout');
	return (path: string, search = '') =>
		(load as (event: { url: URL }) => Promise<void>)({
			url: new URL(`http://tracepad.test${path}${search}`)
		});
}

/** What a SvelteKit `redirect` looks like when it is thrown at us. */
async function redirectOf(run: Promise<unknown>): Promise<string> {
	try {
		await run;
	} catch (thrown) {
		const it = thrown as { status?: number; location?: string };
		if (it.location) return it.location;
		throw thrown;
	}
	throw new Error('nothing was redirected');
}

beforeEach(() => {
	bootstrap.mockReset();
	needsSetup.mockReset();
});

describe('a server with no owner yet', () => {
	it('sends every screen to the setup form', async () => {
		const load = await guard(false, true);

		expect(await redirectOf(load('/traces'))).toBe('/setup');
	});

	it('leaves the setup form alone', async () => {
		const load = await guard(false, true);

		await expect(load('/setup')).resolves.toBeUndefined();
	});
});

describe('a server nobody is signed in to', () => {
	it('sends a guarded screen to the login form with where it was going', async () => {
		const load = await guard(false, false);

		expect(await redirectOf(load('/traces', '?status=error'))).toBe(
			'/login?next=%2Ftraces%3Fstatus%3Derror'
		);
	});

	// Three screens work without a session and only three: signing in, setting
	// up an owner, and accepting an invitation.
	it('leaves the three screens outside the shell alone', async () => {
		const load = await guard(false, false);

		for (const path of ['/login', '/setup', '/invite']) {
			await expect(load(path)).resolves.toBeUndefined();
		}
	});
});

describe('a session', () => {
	it('renders the shell', async () => {
		const load = await guard(true, false);

		await expect(load('/traces')).resolves.toBeUndefined();
	});

	// Setting up an owner signs them in, so the flag that sent them to `/setup`
	// must stop mattering the moment it has been acted on — otherwise the
	// screen it happened on would bounce straight back to it.
	it('outranks a setup flag read before it existed', async () => {
		const load = await guard(true, true);

		await expect(load('/traces')).resolves.toBeUndefined();
	});
});

// The trap this used to be (found in review of PR #54): the boot-time answer
// was trusted forever, so somebody who set the server up and then signed out
// was sent to `/setup` — whose only way onwards is `/login`, which sent them
// straight back. Nothing but a reload escaped it.
describe('a setup flag that has gone stale', () => {
	it('is re-asked, and lets a signed-out visitor reach the login form', async () => {
		const load = await guard(false, true);
		// Somebody used the setup screen — in this tab or in another one.
		needsSetup.mockResolvedValue(false);

		await expect(load('/login')).resolves.toBeUndefined();
		expect(await redirectOf(load('/traces'))).toBe('/login?next=%2Ftraces');
	});

	it('still sends everybody to setup while the server really has no owner', async () => {
		const load = await guard(false, true);

		expect(await redirectOf(load('/traces'))).toBe('/setup');
	});

	// Once the answer is `false` it is settled: a question asked on every
	// guarded navigation forever would be a request per screen.
	it('stops asking once an owner exists', async () => {
		const load = await guard(false, false);

		await load('/login');
		await load('/login');

		expect(needsSetup).not.toHaveBeenCalled();
	});
});

describe('the bootstrap', () => {
	it('runs once, not once per navigation', async () => {
		const load = await guard(true, false);

		await load('/traces');
		await load('/sessions');
		await load('/stats');

		expect(bootstrap).toHaveBeenCalledTimes(1);
	});
});
