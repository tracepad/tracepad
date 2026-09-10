import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('$app/navigation', () => ({ goto: vi.fn(), replaceState: vi.fn() }));

async function fresh() {
	vi.resetModules();
	const { project } = await import('./project.svelte');
	const { auth } = await import('./auth.svelte');
	return { project, auth };
}

const me = (id: string, ...projects: { id: string; name: string; role: 'viewer' | 'editor' }[]) => ({
	account: { id, email: `${id}@example.com`, name: '', owner: false },
	projects
});

const CHECKOUT = { id: 'p1', name: 'checkout', role: 'editor' as const };
const STAGING = { id: 'p2', name: 'staging', role: 'viewer' as const };

beforeEach(() => {
	window.localStorage.clear();
});

describe('which project is on screen', () => {
	// The server sorts `me.projects` by name, so "the first" is a stable
	// answer rather than whatever order the rows came back in.
	it('is the first of them until somebody picks another', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT, STAGING));

		project.restore();

		expect(project.id).toBe('p1');
		expect(project.name).toBe('checkout');
		expect(project.role).toBe('editor');
	});

	it('is nothing at all for an account with no projects', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1'));

		project.restore();

		expect(project.id).toBeNull();
		expect(project.role).toBeNull();
	});

	it('is remembered per account, not per browser', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT, STAGING));
		project.restore();

		project.choose('p2');
		expect(project.id).toBe('p2');

		// Somebody else signs in on the same laptop: their own last choice, or
		// their own first project — never the previous person's.
		auth.adopt(me('acc2', CHECKOUT, STAGING));
		project.restore();

		expect(project.id).toBe('p1');
		expect(window.localStorage.getItem('tracepad.project.acc1')).toBe('p2');
	});

	it('comes back to the same one on the next load', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT, STAGING));
		project.restore();
		project.choose('p2');

		const second = await fresh();
		second.auth.adopt(me('acc1', CHECKOUT, STAGING));
		second.project.restore();

		expect(second.project.id).toBe('p2');
	});

	// A role taken away under an open tab drops the row out of `me.projects`.
	// Pointing the header at it would be a 403 on every request.
	it('falls back when the remembered project is gone', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT, STAGING));
		project.restore();
		project.choose('p2');

		auth.adopt(me('acc1', CHECKOUT));

		expect(project.id).toBe('p1');
	});
});

describe('what the role allows', () => {
	it('opens the editing controls for an editor and an owner', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', CHECKOUT));
		project.restore();

		expect(project.editor).toBe(true);
	});

	it('closes them for a viewer', async () => {
		const { project, auth } = await fresh();
		auth.adopt(me('acc1', STAGING));
		project.restore();

		expect(project.editor).toBe(false);
	});
});
