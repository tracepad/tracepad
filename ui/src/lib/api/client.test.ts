import { beforeEach, describe, expect, it, vi } from 'vitest';

const goto = vi.fn();
const replaceState = vi.fn();
vi.mock('$app/navigation', () => ({ goto, replaceState }));

async function fresh() {
	vi.resetModules();
	const { api, ApiError, search } = await import('./client.svelte');
	const { auth } = await import('$lib/auth.svelte');
	return { api, ApiError, search, auth };
}

/** A fetch that answers one queued response per call, recording the requests. */
function stubFetch(...responses: Response[]) {
	const calls: { url: string; key: string | null }[] = [];
	const queue = [...responses];
	vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
		const headers = new Headers(init?.headers);
		calls.push({ url, key: headers.get('Authorization') });
		const next = queue.shift();
		if (!next) throw new Error(`unexpected request to ${url}`);
		return Promise.resolve(next);
	});
	return calls;
}

const json = (body: unknown, status = 200) =>
	new Response(JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json', 'X-Tracepad-Version': '0.1.0-test' }
	});

beforeEach(() => {
	window.localStorage.clear();
	goto.mockClear();
	vi.unstubAllGlobals();
});

describe('query building', () => {
	it('drops what the API would refuse', async () => {
		const { search } = await fresh();

		// The read API rejects a parameter given without a value on purpose
		// (spec 004 #23), so an untouched filter must not be sent at all.
		expect(search({ name: '', status: undefined, tag: [], environment: 'prod' })).toBe(
			'?environment=prod'
		);
	});

	it('repeats a tag rather than joining it', async () => {
		const { search } = await fresh();

		expect(search({ tag: ['a', 'b'] })).toBe('?tag=a&tag=b');
	});
});

describe('requests', () => {
	it('carries the stored key and records the server version', async () => {
		const { api, auth } = await fresh();
		auth.adopt('tp-sk-live');
		const calls = stubFetch(json({ traces: [], next_cursor: null }));

		await api.listTraces({ status: 'error' }, { limit: 25 });

		expect(calls[0].url).toBe('/api/v1/traces?status=error&limit=25');
		expect(calls[0].key).toBe('Bearer tp-sk-live');
		expect(api.version).toBe('0.1.0-test');
	});

	it('turns a 401 into a cleared key and a trip to login', async () => {
		const { api, ApiError, auth } = await fresh();
		auth.adopt('tp-sk-revoked');
		stubFetch(json({ error: 'unauthorized' }, 401));

		await expect(api.listTraces({})).rejects.toBeInstanceOf(ApiError);

		expect(auth.key).toBeNull();
		expect(goto).toHaveBeenCalledWith('/login', { replaceState: true });
	});

	it("passes the server's own words through on other failures", async () => {
		const { api, auth } = await fresh();
		auth.adopt('tp-sk-live');
		stubFetch(json({ error: 'status must be error or ok, got "boom"' }, 400));

		await expect(api.listTraces({})).rejects.toThrow('status must be error or ok');
		// A 400 is the caller's mistake, not a bad credential.
		expect(auth.key).toBe('tp-sk-live');
	});

	it('reports an unreachable server rather than a status', async () => {
		const { api, ApiError, auth } = await fresh();
		auth.adopt('tp-sk-live');
		vi.stubGlobal('fetch', () => Promise.reject(new TypeError('Failed to fetch')));

		const failure = await api.listTraces({}).catch((cause) => cause);

		expect(failure).toBeInstanceOf(ApiError);
		expect((failure as InstanceType<typeof ApiError>).offline).toBe(true);
	});
});

describe('probing a credential before storing it', () => {
	it('accepts a key that can read traces', async () => {
		const { api } = await fresh();
		const calls = stubFetch(json({ traces: [], next_cursor: null }));

		expect(await api.probe('tp-sk-good')).toBe('project-key');
		// Validated with the request the app actually needs (spec 006 #13).
		expect(calls[0].url).toBe('/api/v1/traces?limit=1');
		expect(calls[0].key).toBe('Bearer tp-sk-good');
	});

	it('tells the admin token apart from a wrong key', async () => {
		const { api } = await fresh();
		stubFetch(json({ error: 'unauthorized' }, 401), json({ projects: [] }));

		expect(await api.probe('admin-token')).toBe('admin-token');
	});

	it('rejects a credential the server knows nothing about', async () => {
		const { api } = await fresh();
		stubFetch(json({ error: 'unauthorized' }, 401), json({ error: 'unauthorized' }, 401));

		expect(await api.probe('nonsense')).toBe('rejected');
	});

	it('never stores or clears anything by itself', async () => {
		const { api, auth } = await fresh();
		auth.adopt('tp-sk-current');
		stubFetch(json({ error: 'unauthorized' }, 401), json({ error: 'unauthorized' }, 401));

		await api.probe('nonsense');

		// Probing a candidate must not sign the current session out.
		expect(auth.key).toBe('tp-sk-current');
		expect(goto).not.toHaveBeenCalled();
	});
});
