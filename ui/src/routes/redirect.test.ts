import { beforeEach, describe, expect, it, vi } from 'vitest';

// Bare paths redirect (spec 029 #3): `/`, and every path that is not `/p/…`
// and not one of the three screens outside the shell, goes to the same path
// and query under the remembered project. An account that reaches no
// project is sent to `/p` (#4), and `/p` itself goes on to the project's
// traces for everybody else.

vi.mock('$app/navigation', () => ({ goto: vi.fn(), replaceState: vi.fn() }));
vi.mock('$app/state', () => ({ page: { params: {} } }));

const P1 = 'a'.repeat(32);
const P2 = 'b'.repeat(32);

type Load = (event: {
	parent: () => Promise<void>;
	url: URL;
	params: Record<string, string>;
}) => Promise<void>;

/** A session that reaches the given projects, with the redirect loads on top. */
async function signedIn(...projects: { id: string; name: string }[]) {
	vi.resetModules();
	const { auth } = await import('$lib/auth.svelte');
	auth.adopt({
		account: { id: 'acc1', email: 'her@example.com', name: '', owner: false },
		projects: projects.map((one) => ({ ...one, role: 'editor' as const }))
	});
	const { project } = await import('$lib/project.svelte');
	const root = (await import('./+page')).load as unknown as Load;
	const rest = (await import('./[...path]/+page')).load as unknown as Load;
	const landing = (await import('./p/+page')).load as unknown as Load;
	const account = (await import('./p/[project]/settings/account/+page')).load as unknown as Load;
	const parent = () => Promise.resolve();
	// Through a promise either way: a load that redirects before its first
	// `await` throws synchronously, and this is what SvelteKit does with it.
	const at = (load: Load, path: string, params: Record<string, string> = {}) =>
		Promise.resolve().then(() =>
			load({ parent, url: new URL(`http://tracepad.test${path}`), params })
		);
	return {
		project,
		root: at.bind(null, root),
		rest: at.bind(null, rest),
		landing: at.bind(null, landing),
		account: at.bind(null, account)
	};
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
	window.localStorage.clear();
});

describe('a bare path', () => {
	it('lands on the same path and query under the remembered project', async () => {
		const { project, rest } = await signedIn({ id: P1, name: 'checkout' }, { id: P2, name: 'staging' });
		project.remember(P2);

		expect(await redirectOf(rest('/traces?status=error'))).toBe(`/p/${P2}/traces?status=error`);
		expect(await redirectOf(rest('/settings/account'))).toBe(`/p/${P2}/settings/account`);
	});

	it('lands on the first project by name when nothing is remembered', async () => {
		const { rest } = await signedIn({ id: P1, name: 'checkout' }, { id: P2, name: 'staging' });

		expect(await redirectOf(rest('/sessions'))).toBe(`/p/${P1}/sessions`);
	});

	// The rest route also catches what is under the prefix and names no
	// screen. Prefixing that again would be a redirect chasing its own tail.
	it('is a 404, not a second prefix, once it already has one', async () => {
		const { rest } = await signedIn({ id: P1, name: 'checkout' });

		await expect(rest(`/p/${P1}/nonsense`)).rejects.toMatchObject({ status: 404 });
		await expect(rest(`/p/${P1}/traces/a/b`)).rejects.toMatchObject({ status: 404 });
	});
});

// The Account tab is about the person and lives bare (spec 029 #14): a link
// written under a prefix lands on it with its query; a bare one is its own
// route and never reaches the rest route — so an account that reaches no
// project, which every bare path sends to `/p`, still opens it.
describe('the Account tab', () => {
	it('is reached bare from under any prefix', async () => {
		const { account } = await signedIn({ id: P1, name: 'checkout' });

		expect(await redirectOf(account(`/p/${P1}/settings/account?x=1`, { project: P1 }))).toBe(
			'/settings/account?x=1'
		);
		expect(await redirectOf(account('/p/not-mine/settings/account', { project: 'not-mine' }))).toBe(
			'/settings/account'
		);
	});
});

describe('the root', () => {
	it('is the remembered project’s traces', async () => {
		const { project, root } = await signedIn({ id: P1, name: 'checkout' }, { id: P2, name: 'staging' });
		project.remember(P2);

		expect(await redirectOf(root('/'))).toBe(`/p/${P2}/traces`);
	});
});

describe('an account with no projects', () => {
	it('is sent to /p from anywhere', async () => {
		const { root, rest } = await signedIn();

		expect(await redirectOf(root('/'))).toBe('/p');
		expect(await redirectOf(rest('/traces?status=error'))).toBe('/p');
	});

	it('stays on /p, which everybody else is sent on from', async () => {
		const nobody = await signedIn();
		await expect(nobody.landing('/p')).resolves.toBeUndefined();

		const somebody = await signedIn({ id: P1, name: 'checkout' });
		expect(await redirectOf(somebody.landing('/p'))).toBe(`/p/${P1}/traces`);
	});
});
