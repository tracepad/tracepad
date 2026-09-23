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

/** A signed-in session with one project, which is what most screens run on. */
const ME = {
	account: { id: 'acc1', email: 'her@example.com', name: '', owner: false, preferences: {} },
	projects: [{ id: 'p1', name: 'checkout', role: 'editor' as const }]
};

/** A fetch that answers one queued response per call, recording the requests. */
function stubFetch(...responses: Response[]) {
	const calls: { url: string; credentials?: RequestCredentials; headers: Headers }[] = [];
	const queue = [...responses];
	vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
		calls.push({ url, credentials: init?.credentials, headers: new Headers(init?.headers) });
		const next = queue.shift();
		if (!next) throw new Error(`unexpected request to ${url}`);
		return Promise.resolve(next);
	});
	return calls;
}

/**
 * A fetch that never answers on its own — only the signal ends it, the way a
 * real one rejects with the signal's reason once aborted.
 */
function hangingFetch() {
	const calls: { signal: AbortSignal }[] = [];
	vi.stubGlobal(
		'fetch',
		(_url: string, init?: RequestInit) =>
			new Promise((_, reject) => {
				const signal = init!.signal!;
				calls.push({ signal });
				signal.addEventListener('abort', () => reject(signal.reason));
			})
	);
	return calls;
}

/**
 * `AbortSignal.timeout` over the global `setTimeout`, so that fake timers can
 * fire it. Vitest hands jsdom Node's `AbortSignal`, and Node's schedules its
 * timer on the `timers` module directly, out of `vi.useFakeTimers`'s reach.
 * The product keeps the real one; this is the clock the test can turn.
 */
function fakeTimeout() {
	vi.spyOn(AbortSignal, 'timeout').mockImplementation((ms) => {
		const controller = new AbortController();
		const reason = new DOMException('The operation timed out.', 'TimeoutError');
		setTimeout(() => controller.abort(reason), ms);
		return controller.signal;
	});
}

const json = (body: unknown, status = 200) =>
	new Response(JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json', 'X-Tracepad-Version': '0.1.0-test' }
	});

beforeEach(() => {
	window.localStorage.clear();
	goto.mockClear();
	window.history.replaceState(null, '', '/traces');
	vi.unstubAllGlobals();
	vi.restoreAllMocks();
	vi.useRealTimers();
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
	it('travels on the cookie and records the server version', async () => {
		const { api, auth } = await fresh();
		auth.adopt(ME);
		const calls = stubFetch(json({ traces: [], next_cursor: null }));

		await api.listTraces({ status: 'error' }, { limit: 25 });

		expect(calls[0].url).toBe('/api/v1/traces?status=error&limit=25');
		// The credential is the session cookie the browser holds; there is no
		// Authorization header for the interface to send any more (#13).
		expect(calls[0].credentials).toBe('same-origin');
		expect(calls[0].headers.get('Authorization')).toBeNull();
		expect(api.version).toBe('0.1.0-test');
	});

	it('turns a 401 into a sign-out and a trip to login with where we were', async () => {
		const { api, ApiError, auth } = await fresh();
		auth.adopt(ME);
		window.history.replaceState(null, '', '/traces?status=error');
		stubFetch(json({ error: 'unauthorized' }, 401));

		await expect(api.listTraces({})).rejects.toBeInstanceOf(ApiError);

		expect(auth.signedIn).toBe(false);
		expect(goto).toHaveBeenCalledWith('/login?next=%2Ftraces%3Fstatus%3Derror', {
			replaceState: true
		});
	});

	it("leaves the guard's own 401 to the guard", async () => {
		const { api, ApiError, auth } = await fresh();
		stubFetch(json({ error: 'unauthorized' }, 401));

		// `me` is how the shell asks whether anybody is signed in. A 401 is the
		// answer, not an accident, so it must not race the redirect the guard
		// is about to make.
		await expect(api.me()).rejects.toBeInstanceOf(ApiError);

		expect(auth.signedIn).toBe(false);
		expect(goto).not.toHaveBeenCalled();
	});

	it("passes the server's own words through on other failures", async () => {
		const { api, auth } = await fresh();
		auth.adopt(ME);
		stubFetch(json({ error: 'status must be error or ok, got "boom"' }, 400));

		await expect(api.listTraces({})).rejects.toThrow('status must be error or ok');
		// A 400 is the caller's mistake, not an ended session.
		expect(auth.signedIn).toBe(true);
	});

	it('reads a 204 as an answer rather than as broken JSON', async () => {
		const { api, auth } = await fresh();
		auth.adopt(ME);
		stubFetch(new Response(null, { status: 204 }));

		await expect(api.deleteMembership('acc1', 'p1')).resolves.toBeUndefined();
	});

	it('reports an unreachable server rather than a status', async () => {
		const { api, ApiError, auth } = await fresh();
		auth.adopt(ME);
		vi.stubGlobal('fetch', () => Promise.reject(new TypeError('Failed to fetch')));

		const failure = await api.listTraces({}).catch((cause) => cause);

		expect(failure).toBeInstanceOf(ApiError);
		expect((failure as InstanceType<typeof ApiError>).offline).toBe(true);
	});

	it('gives up on a request that never answers, as it would on an unreachable server', async () => {
		const { api, ApiError, auth } = await fresh();
		auth.adopt(ME);
		vi.useFakeTimers();
		fakeTimeout();
		const calls = hangingFetch();

		const failure = api.listTraces({}).catch((cause) => cause);

		// Thirty seconds, in one place, on every request (spec 010 #10).
		vi.advanceTimersByTime(29_999);
		expect(calls[0].signal.aborted).toBe(false);
		vi.advanceTimersByTime(1);
		const cause = await failure;

		// Not an abort: an abort is the caller saying the question went stale
		// and lands nowhere (spec 010 #8). This is the server not answering,
		// which a load shows as its failure and a tick in its own slot.
		expect(cause).toBeInstanceOf(ApiError);
		expect((cause as InstanceType<typeof ApiError>).offline).toBe(true);
		expect((cause as Error).message).toBe('the server did not answer in time');
	});

	it('lets a media body take its time once the headers are in, but not the headers', async () => {
		const { api, ApiError, auth } = await fresh();
		auth.adopt(ME);
		vi.useFakeTimers();
		fakeTimeout();
		// Twenty megabytes on a slow link: the headers are prompt, the body
		// takes a minute (spec 041).
		vi.stubGlobal('fetch', (_url: string, init?: RequestInit) => {
			const signal = init!.signal!;
			const response = new Response(null, { status: 200 });
			// A real body read rejects with the signal's reason, as this does.
			response.blob = () =>
				new Promise((resolve, reject) => {
					signal.addEventListener('abort', () => reject(signal.reason));
					setTimeout(() => resolve(new Blob(['x'])), 60_000);
				});
			return Promise.resolve(response);
		});
		const body = api.media('a'.repeat(64));
		await vi.advanceTimersByTimeAsync(60_000);
		expect(await body).toBeInstanceOf(Blob);

		// A server that sends no headers is still given up on.
		hangingFetch();
		const failure = api.media('b'.repeat(64)).catch((cause) => cause);
		await vi.advanceTimersByTimeAsync(30_000);
		const cause = await failure;
		expect(cause).toBeInstanceOf(ApiError);
		expect((cause as Error).message).toBe('the server did not answer in time');
	});

	it('gives up the same way on a body that never finishes', async () => {
		const { api, ApiError, auth } = await fresh();
		auth.adopt(ME);
		vi.useFakeTimers();
		fakeTimeout();
		// The headers arrive; the body is the part that stalls. The clock covers
		// both, and a real body read rejects with the signal's reason too.
		vi.stubGlobal('fetch', (_url: string, init?: RequestInit) => {
			const signal = init!.signal!;
			const response = new Response(null, { status: 200 });
			response.json = () =>
				new Promise((_, reject) => signal.addEventListener('abort', () => reject(signal.reason)));
			return Promise.resolve(response);
		});

		const failure = api.listTraces({}).catch((cause) => cause);
		await vi.advanceTimersByTimeAsync(30_000);
		const cause = await failure;

		expect(cause).toBeInstanceOf(ApiError);
		expect((cause as InstanceType<typeof ApiError>).offline).toBe(true);
		expect((cause as Error).message).toBe('the server did not answer in time');
	});

	it("passes the caller's own abort through untouched", async () => {
		const { api, ApiError, auth } = await fresh();
		auth.adopt(ME);
		const calls = hangingFetch();
		const controller = new AbortController();

		const failure = api.listTraces({}, {}, controller.signal).catch((cause) => cause);
		controller.abort();
		const cause = await failure;

		expect(calls[0].signal.aborted).toBe(true);
		expect(cause).not.toBeInstanceOf(ApiError);
		expect((cause as Error).name).toBe('AbortError');
	});
});
