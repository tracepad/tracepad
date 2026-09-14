import { beforeEach, describe, expect, it, vi } from 'vitest';

// The project on screen is the one in the URL (spec 029 #2), every link the
// interface writes is built under it, and a switch keeps the question and
// drops the answer (#6). The route parameter is mocked, because it is the one
// input all of this derives from.

const params = { current: {} as { project?: string } };
vi.mock('$app/navigation', () => ({ goto: vi.fn(), replaceState: vi.fn() }));
vi.mock('$app/state', () => ({
	page: {
		get params() {
			return params.current;
		}
	}
}));

async function fresh() {
	vi.resetModules();
	const project = await import('./project.svelte');
	const { auth } = await import('./auth.svelte');
	return { ...project, auth };
}

const me = (id: string, ...projects: { id: string; name: string; role: 'viewer' | 'editor' }[]) => ({
	account: { id, email: `${id}@example.com`, name: '', owner: false },
	projects
});

const P1 = 'a'.repeat(32);
const P2 = 'b'.repeat(32);
const CHECKOUT = { id: P1, name: 'checkout', role: 'editor' as const };
const STAGING = { id: P2, name: 'staging', role: 'viewer' as const };

beforeEach(() => {
	window.localStorage.clear();
	params.current = {};
});

describe('which project is on screen', () => {
	it('follows the URL', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT, STAGING));

		params.current = { project: P2 };

		expect(project.id).toBe(P2);
		expect(project.name).toBe('staging');
		expect(project.role).toBe('viewer');
	});

	// Unknown, malformed, or a membership taken away under an open tab: the
	// interface cannot tell and does not try (#4), and no header names it.
	it('is nothing for an id the account cannot reach', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT));

		for (const id of [P2, 'not-a-project', '']) {
			params.current = { project: id };
			expect(project.current).toBeNull();
			expect(project.id).toBeNull();
			expect(project.role).toBeNull();
		}
	});

	it('is nothing outside a project', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT));

		expect(project.id).toBeNull();
	});

	it('opens the editing controls for an editor and closes them for a viewer', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT, STAGING));

		params.current = { project: P1 };
		expect(project.editor).toBe(true);
		params.current = { project: P2 };
		expect(project.editor).toBe(false);
	});
});

describe('the remembered project', () => {
	it('is the first by name until a screen has been opened', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', STAGING, CHECKOUT));

		expect(project.remembered()).toBe(P2);
	});

	it('survives a reload', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT, STAGING));
		project.remember(P2);

		const second = await fresh();
		second.auth.adopt(me('acc1', CHECKOUT, STAGING));

		expect(second.project.remembered()).toBe(P2);
	});

	it('is ignored once the account cannot reach it', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT, STAGING));
		project.remember(P2);

		auth.adopt(me('acc1', CHECKOUT));

		expect(project.remembered()).toBe(P1);
	});

	it('is nothing for an account with no projects', async () => {
		const { project, bareTarget, auth } = await fresh();
		auth.adopt(me('acc1'));

		expect(project.remembered()).toBeNull();
		expect(bareTarget('/traces')).toBe('/p');
	});

	// The redirect of #3: the same path and query, under the remembered one.
	it('is where a bare path is sent', async () => {
		const { project, bareTarget, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT, STAGING));
		project.remember(P2);

		expect(bareTarget('/traces', '?status=error')).toBe(`/p/${P2}/traces?status=error`);
		expect(bareTarget('/traces')).toBe(`/p/${P2}/traces`);
	});

	// Two people who share a laptop must not swap each other's project.
	it('is per account, not per browser', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT, STAGING));
		project.remember(P2);

		auth.adopt(me('acc2', CHECKOUT, STAGING));

		expect(project.remembered()).toBe(P1);
		expect(window.localStorage.getItem('tracepad.project.acc1')).toBe(P2);
	});
});

describe('href', () => {
	it('prefixes every section path with the project on screen', async () => {
		const { href } = await fresh();
		params.current = { project: P1 };

		for (const path of [
			'/traces',
			'/sessions',
			'/users',
			'/stats',
			'/prompts',
			'/datasets',
			'/runs',
			'/score-configs',
			'/queues',
			'/quality',
			'/settings/account'
		]) {
			expect(href(path)).toBe(`/p/${P1}${path}`);
		}
	});

	it('keeps a query it is given, as a string or as params', async () => {
		const { href } = await fresh();
		params.current = { project: P1 };

		expect(href('/traces', '?status=error')).toBe(`/p/${P1}/traces?status=error`);
		expect(href('/traces', 'status=error')).toBe(`/p/${P1}/traces?status=error`);
		expect(href('/traces', new URLSearchParams({ q: 'a b' }))).toBe(`/p/${P1}/traces?q=a+b`);
		expect(href('/traces?limit=25', 'status=error')).toBe(`/p/${P1}/traces?limit=25&status=error`);
		expect(href('/traces?prompt=x')).toBe(`/p/${P1}/traces?prompt=x`);
		expect(href('/traces', '')).toBe(`/p/${P1}/traces`);
	});

	// The no-projects screen and the error page have no project to write;
	// the bare path stays bare and the redirect takes it from there (#3).
	it('leaves the path bare outside a project', async () => {
		const { href } = await fresh();

		expect(href('/traces')).toBe('/traces');
	});
});

// The drill-down of spec 028 #25: a row of the Server tab's table leads to
// that project's Project tab, and the Project tab leads back to the table
// of whichever project is on screen.
describe('the projects table', () => {
	it('leads into another project and back', async () => {
		const { href, under } = await fresh();
		params.current = { project: P1 };

		expect(under('/settings/project', P2)).toBe(`/p/${P2}/settings/project`);
		params.current = { project: P2 };
		expect(href('/settings/server')).toBe(`/p/${P2}/settings/server`);
	});
});

describe('within', () => {
	it('takes the prefix off and leaves a bare path alone', async () => {
		const { within } = await fresh();

		expect(within(`/p/${P1}/settings/server`)).toBe('/settings/server');
		expect(within(`/p/${P1}`)).toBe('/');
		expect(within('/p')).toBe('/p');
		expect(within('/login')).toBe('/login');
	});
});

describe('switchTarget', () => {
	const at = (path: string) => new URL(`http://tracepad.test${path}`);

	// One case per section: the section stays, everything deeper goes.
	it.each([
		['/traces', '/traces'],
		[`/traces/${'c'.repeat(32)}`, '/traces'],
		['/sessions', '/sessions'],
		['/sessions/s-1', '/sessions'],
		['/users', '/users'],
		['/users/u-1', '/users'],
		['/stats', '/stats'],
		['/prompts', '/prompts'],
		['/prompts/support-answer/versions/new', '/prompts'],
		['/prompts/new', '/prompts'],
		['/datasets', '/datasets'],
		['/datasets/golden/items/i-1/edit', '/datasets'],
		['/datasets/items/new', '/datasets'],
		['/runs', '/runs'],
		['/runs/r-1', '/runs'],
		['/runs/r-1/compare/r-2', '/runs'],
		['/score-configs', '/score-configs'],
		['/queues', '/queues'],
		['/queues/review/annotate', '/queues'],
		['/quality', '/quality'],
		['/settings', '/settings'],
		['/settings/account', '/settings/account'],
		['/settings/server', '/settings/server'],
		['', '/traces'],
		// A stale link's 404 is not a section to keep: the switch is the exit.
		['/nonsense', '/traces'],
		['/nonsense/deeper', '/traces']
	])('%s lands on %s of the other project', async (from, to) => {
		const { switchTarget } = await fresh();

		expect(switchTarget(at(`/p/${P1}${from}`), P2)).toBe(`/p/${P2}${to}`);
	});

	// A project made from the Server tab is opened on the same tab (#15): the
	// rule of #6 applied to the project just made rather than one chosen.
	it('is where a project made from the Server tab lands', async () => {
		const { switchTarget } = await fresh();

		expect(switchTarget(at(`/p/${P1}/settings/server`), P2)).toBe(`/p/${P2}/settings/server`);
	});

	// The bare Account tab and `/p` are under no project: nothing to keep.
	it('lands on the traces from a screen that is under no project', async () => {
		const { switchTarget } = await fresh();

		expect(switchTarget(at('/settings/account'), P2)).toBe(`/p/${P2}/traces`);
		expect(switchTarget(at('/p'), P2)).toBe(`/p/${P2}/traces`);
	});

	it('keeps the filters, the range, the size, the search and live mode', async () => {
		const { switchTarget } = await fresh();
		const query = '?status=error&environment=prod&from=2026-09-01T00:00:00Z&limit=25&q=refund&live=1';

		const target = new URL(switchTarget(at(`/p/${P1}/traces${query}`), P2), 'http://tracepad.test');
		expect(target.pathname).toBe(`/p/${P2}/traces`);
		expect([...target.searchParams]).toEqual([...new URLSearchParams(query)]);
	});

	// A cursor, a peeked row, a drilled trace and a selected observation all
	// name something in the old project; the other would 404 on every one.
	it('drops the page position and the peek panel', async () => {
		const { switchTarget } = await fresh();

		expect(
			switchTarget(
				at(`/p/${P1}/sessions?environment=prod&cursor=abc&direction=prev&peek=s-1&trace=t-1&obs=o-1`),
				P2
			)
		).toBe(`/p/${P2}/sessions?environment=prod`);
	});
});
