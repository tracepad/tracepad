import { beforeEach, describe, expect, it, vi } from 'vitest';

const goto = vi.fn();
const replaceState = vi.fn();
vi.mock('$app/navigation', () => ({ goto, replaceState }));

/**
 * The invariant spec 007 #3 is built on, held by a fetch spy: the admin token
 * goes to the management endpoints that demand it and nowhere else, and it is
 * never the credential on anything that reads trace data.
 *
 * This is the test the Decision asks for by name. Two credentials in one app
 * is exactly the shape where "it only goes there" quietly stops being true —
 * one call site with the wrong default and the management-plane token is on
 * every listing request in the tab.
 */

async function fresh() {
	vi.resetModules();
	const { api, ApiError } = await import('./client.svelte');
	const { auth } = await import('$lib/auth.svelte');
	const { admin } = await import('$lib/admin.svelte');
	auth.adopt(PROJECT_KEY);
	admin.adopt(ADMIN_TOKEN);
	return { api, ApiError, auth, admin };
}

const PROJECT_KEY = 'tp-sk-project';
const ADMIN_TOKEN = 'admin-token-secret';
const PROJECT_ID = 'a'.repeat(32);

type Call = { url: string; method: string; key: string | null };

/** A fetch that answers everything with `body`, recording who asked and how. */
function spyFetch(body: unknown = {}, status = 200): Call[] {
	const calls: Call[] = [];
	vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
		const headers = new Headers(init?.headers);
		calls.push({
			url,
			method: init?.method ?? 'GET',
			key: headers.get('Authorization')
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
	goto.mockClear();
	vi.unstubAllGlobals();
});

describe('the data plane', () => {
	it('never carries the admin token', async () => {
		const { api } = await fresh();
		const calls = spyFetch({ traces: [], sessions: [], buckets: [], next_cursor: null });

		await api.listTraces({});
		await api.listSessions({});
		await api.getSession('s1');
		await api.getStats({ group_by: 'day' });
		await api.getTrace('b'.repeat(32));
		await api.listProjects();

		expect(calls).toHaveLength(6);
		for (const call of calls) {
			expect(call.key).toBe(`Bearer ${PROJECT_KEY}`);
			expect(call.key).not.toContain(ADMIN_TOKEN);
		}
	});
});

describe("the project's own management", () => {
	it('runs on the project key, which is what administers its own project', async () => {
		const { api } = await fresh();
		const calls = spyFetch({ keys: [], dry_run: false, deleted: {} });

		await api.listKeys(PROJECT_ID);
		await api.createKey(PROJECT_ID);
		await api.revokeKey(PROJECT_ID, 'tp-pk-old');
		await api.eraseUserData(PROJECT_ID, 'u1');
		await api.patchProject(PROJECT_ID, { retention_days: 30 });

		for (const call of calls) {
			expect(call.key).toBe(`Bearer ${PROJECT_KEY}`);
		}
		// And the destructive ones are the endpoint's own dry run until the
		// echo is passed back (spec 005 #8, spec 007 #5).
		expect(calls[2].url).toBe(`/api/v1/projects/${PROJECT_ID}/keys/tp-pk-old`);
		expect(calls[2].method).toBe('DELETE');
		expect(calls[3].url).toBe(`/api/v1/projects/${PROJECT_ID}/users/u1/data`);
		expect(calls[4].method).toBe('PATCH');
	});

	it('sends the echo the server asked for when there is one', async () => {
		const { api } = await fresh();
		const calls = spyFetch({ dry_run: false, deleted: {} });

		await api.eraseUserData(PROJECT_ID, 'u1', 'u1');
		await api.patchProject(PROJECT_ID, { retention_days: 1 }, 'my-project');

		expect(calls[0].url).toContain('confirm=u1');
		expect(calls[1].url).toContain('confirm=my-project');
	});
});

describe('the management plane', () => {
	it('is the only place the admin token is sent', async () => {
		const { api } = await fresh();
		const calls = spyFetch({ projects: [], id: PROJECT_ID, name: 'x' });

		await api.listAllProjects();
		await api.createProject('staging');
		await api.renameProject(PROJECT_ID, 'renamed');
		await api.deleteProject(PROJECT_ID);
		await api.restoreProject(PROJECT_ID);

		expect(calls).toHaveLength(5);
		for (const call of calls) {
			expect(call.key).toBe(`Bearer ${ADMIN_TOKEN}`);
			// Every one of them is under /projects: the token reaches the
			// lifecycle and nothing else (spec 007 #4).
			expect(call.url.startsWith('/api/v1/projects')).toBe(true);
		}
		// Listing soft-deleted projects is the one request only this token can
		// make, which is what makes it a usable probe.
		expect(calls[0].url).toContain('include=deleted');
	});

	it('refuses to send a request it has no token for', async () => {
		const { api, ApiError, admin } = await fresh();
		admin.clear();
		const calls = spyFetch({ projects: [] });

		await expect(api.listAllProjects()).rejects.toBeInstanceOf(ApiError);

		// Not attempted at all, rather than attempted with the project key —
		// which would send the session's credential somewhere it was never
		// meant to go and read a 403 as if it meant something.
		expect(calls).toHaveLength(0);
	});

	it('locks itself on a 401 without signing the reader out', async () => {
		const { api, auth, admin } = await fresh();
		spyFetch({ error: 'unauthorized' }, 401);

		await expect(api.listAllProjects()).rejects.toThrow('admin token');

		expect(admin.unlocked).toBe(false);
		// A bad admin token is not a bad project key: the screens keep working.
		expect(auth.key).toBe(PROJECT_KEY);
		expect(goto).not.toHaveBeenCalled();
	});

	it('probes a candidate token without storing it', async () => {
		const { api, admin } = await fresh();
		admin.clear();
		const calls = spyFetch({ projects: [] });

		expect(await api.probeAdmin('candidate')).toBe(true);

		expect(calls[0].key).toBe('Bearer candidate');
		expect(calls[0].url).toBe('/api/v1/projects?include=deleted');
		// Probing decides; adopting is the caller's move.
		expect(admin.unlocked).toBe(false);
	});

	it('reads a refusal as "not the admin token"', async () => {
		const { api } = await fresh();
		// What a perfectly valid project key gets from `?include=deleted`.
		spyFetch({ error: 'this needs the cross-project admin token' }, 403);

		expect(await api.probeAdmin(PROJECT_KEY)).toBe(false);
	});
});
