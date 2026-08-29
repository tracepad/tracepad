import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('$app/navigation', () => ({ goto: vi.fn(), replaceState: vi.fn() }));

async function fresh() {
	vi.resetModules();
	const { project } = await import('./project.svelte');
	const { auth } = await import('./auth.svelte');
	return { project, auth };
}

function answers(...names: string[]) {
	const calls: string[] = [];
	vi.stubGlobal('fetch', (_url: string, init?: RequestInit) => {
		calls.push(new Headers(init?.headers).get('Authorization') ?? '');
		const name = names[calls.length - 1];
		return Promise.resolve(
			new Response(JSON.stringify({ projects: name ? [{ id: 'x', name }] : [] }), {
				status: 200,
				headers: { 'Content-Type': 'application/json' }
			})
		);
	});
	return calls;
}

beforeEach(() => {
	window.localStorage.clear();
	vi.unstubAllGlobals();
});

describe('the project name in the sidebar', () => {
	it('is read once per credential, not once per session', async () => {
		const { project, auth } = await fresh();
		const calls = answers('first', 'second');

		auth.adopt('tp-sk-one');
		await project.load();
		expect(project.name).toBe('first');

		// Asking again with the same key must not spend a request.
		await project.load();
		expect(calls).toHaveLength(1);

		// A 401 signs the reader out without going through the sidebar, so a
		// new key can be a different project's — and the old name must go.
		auth.reject();
		auth.adopt('tp-sk-two');
		await project.load();

		expect(project.name).toBe('second');
		expect(calls).toEqual(['Bearer tp-sk-one', 'Bearer tp-sk-two']);
	});

	it('asks again after a failure rather than staying blank forever', async () => {
		const { project, auth } = await fresh();
		auth.adopt('tp-sk-one');
		vi.stubGlobal('fetch', () => Promise.reject(new TypeError('offline')));

		await project.load();
		expect(project.name).toBeNull();

		answers('recovered');
		await project.load();
		expect(project.name).toBe('recovered');
	});

	it('says nothing at all without a credential', async () => {
		const { project } = await fresh();
		const calls = answers('never');

		await project.load();

		expect(project.name).toBeNull();
		expect(calls).toHaveLength(0);
	});
});

describe('re-reading the project after a change', () => {
	it('never blanks the row on the way', async () => {
		const { project, auth } = await fresh();
		answers('before', 'after');
		auth.adopt('tp-sk-one');
		await project.load();

		const inFlight = project.refresh();
		// Settings renders its cards only when there is a project, so a row
		// that disappeared for one tick would tear every card down and build
		// it again — taking with it the confirmation the reader was reading.
		expect(project.current?.name).toBe('before');

		await inFlight;
		expect(project.current?.name).toBe('after');
	});
});

