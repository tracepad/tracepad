import { beforeEach, describe, expect, it, vi } from 'vitest';

const goto = vi.fn();
const replaceState = vi.fn();
vi.mock('$app/navigation', () => ({ goto, replaceState }));

/**
 * What every request carries now that a person signs in (spec 028 #4, #6): the
 * session cookie and nothing else, plus the id of the project the screen is
 * about — on the routes that need it, and on no others.
 *
 * This is the fetch-spy that spec 007 #3 had for the admin token, pointed at
 * the invariant that replaced it. A session is not a project the way a key was,
 * so "which project is this about" is a claim one function makes for the whole
 * client, and one call site sending it to the wrong place is exactly the shape
 * of bug that would leak a listing across projects.
 */

const PROJECT = 'a'.repeat(32);
const OTHER = 'b'.repeat(32);

// The project on screen is the one in the URL (spec 029 #2), which the client
// reads off the route parameter.
const params = { current: { project: PROJECT } as { project?: string } };
vi.mock('$app/state', () => ({
	page: {
		get params() {
			return params.current;
		},
		get url() {
			return new URL(`http://tracepad.test/p/${params.current.project ?? ''}/traces`);
		}
	}
}));

const ME = {
	account: { id: 'acc1', email: 'her@example.com', name: 'Her', owner: true },
	projects: [
		{ id: PROJECT, name: 'checkout', role: 'owner' as const },
		{ id: OTHER, name: 'staging', role: 'owner' as const }
	]
};

async function fresh() {
	vi.resetModules();
	const { api, ApiError } = await import('./client.svelte');
	const { auth } = await import('$lib/auth.svelte');
	const { project } = await import('$lib/project.svelte');
	auth.adopt(ME);
	params.current = { project: PROJECT };
	return { api, ApiError, auth, project };
}

type Call = { url: string; method: string; project: string | null; authorization: string | null };

/** A fetch that answers everything with `body`, recording who asked and how. */
function spyFetch(body: unknown = {}, status = 200): Call[] {
	const calls: Call[] = [];
	vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
		const headers = new Headers(init?.headers);
		expect(init?.credentials).toBe('same-origin');
		calls.push({
			url,
			method: init?.method ?? 'GET',
			project: headers.get('X-Tracepad-Project'),
			authorization: headers.get('Authorization')
		});
		return Promise.resolve(
			new Response(JSON.stringify(body), {
				status,
				headers: { 'Content-Type': 'application/json' }
			})
		);
	});
	return calls;
}

beforeEach(() => {
	window.localStorage.clear();
	window.history.replaceState(null, '', '/traces');
	goto.mockClear();
	vi.unstubAllGlobals();
});

describe('the data plane', () => {
	it('names the project on every request and holds no credential of its own', async () => {
		const { api } = await fresh();
		const calls = spyFetch({ traces: [], sessions: [], buckets: [], next_cursor: null });

		await api.listTraces({});
		await api.listSessions({});
		await api.getSession('s1');
		await api.getStats({ group_by: 'day' });
		await api.getTrace('c'.repeat(32));
		await api.listPrompts();
		await api.listQueues();

		expect(calls).toHaveLength(7);
		for (const call of calls) {
			expect(call.project).toBe(PROJECT);
			expect(call.authorization).toBeNull();
		}
	});

	it('follows the project in the URL', async () => {
		const { api } = await fresh();
		params.current = { project: OTHER };
		const calls = spyFetch({ traces: [], next_cursor: null });

		await api.listTraces({});

		expect(calls[0].project).toBe(OTHER);
	});

	// An id the account cannot reach is no project at all (spec 029 #4): no
	// header, rather than one the server would answer 403 to.
	it('names nothing for a project the account cannot reach', async () => {
		const { api } = await fresh();
		params.current = { project: 'c'.repeat(32) };
		const calls = spyFetch({ traces: [], next_cursor: null });

		await api.listTraces({});

		expect(calls[0].project).toBeNull();
	});
});

describe('the routes that carry their own project', () => {
	it('are sent no header at all', async () => {
		const { api } = await fresh();
		const calls = spyFetch({ projects: [], keys: [], accounts: [], sessions: [], dry_run: false });

		await api.listProjects();
		await api.getProject(PROJECT);
		await api.listKeys(PROJECT);
		await api.me();
		await api.listSignIns();
		await api.listAccounts();
		await api.getSetup();

		// A session sends no `X-Tracepad-Project` on any of these (Decision 19):
		// the project is in the path, or the question is not about one.
		for (const call of calls) expect(call.project).toBeNull();
		expect(calls).toHaveLength(7);
	});
});

describe('what the interface can no longer do', () => {
	it('has no admin token to send', async () => {
		const { api } = await fresh();

		// The five lifecycle calls used to be the only ones carrying a second
		// credential (spec 007 #3). They are an owner's session now, so the
		// grep that used to find `scope: 'admin'` finds nothing.
		expect('probeAdmin' in api).toBe(false);
		expect('probe' in api).toBe(false);
	});

	it('runs the project lifecycle on the session', async () => {
		const { api } = await fresh();
		const calls = spyFetch({ projects: [], id: PROJECT, name: 'x' });

		await api.listAllProjects();
		await api.createProject('staging');
		await api.renameProject(PROJECT, 'renamed');
		await api.deleteProject(PROJECT);
		await api.restoreProject(PROJECT);

		expect(calls).toHaveLength(5);
		for (const call of calls) {
			expect(call.authorization).toBeNull();
			expect(call.url.startsWith('/api/v1/projects')).toBe(true);
		}
		expect(calls[0].url).toContain('include=deleted');
	});
});

describe('the destructive endpoints', () => {
	it('send the echo the server asked for when there is one', async () => {
		const { api } = await fresh();
		const calls = spyFetch({ dry_run: false, deleted: {} });

		await api.eraseUserData(PROJECT, 'u1', 'u1');
		await api.deleteAccount('acc9', 'helper@example.com');

		expect(calls[0].url).toContain('confirm=u1');
		expect(calls[1].url).toContain('confirm=helper%40example.com');
	});
});
