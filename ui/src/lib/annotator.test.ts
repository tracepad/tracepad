import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('$app/navigation', () => ({ goto: vi.fn(), replaceState: vi.fn() }));

// Who the desk signs as (spec 048 #11): the account when somebody is signed
// in, and the name kept in the browser only when nobody is.

async function fresh() {
	vi.resetModules();
	const { auth } = await import('./auth.svelte');
	const { annotator } = await import('./annotator.svelte');
	return { auth, annotator };
}

const me = (name: string) => ({
	account: { id: 'acc1', email: 'ada@example.com', name, owner: false, preferences: {} },
	projects: []
});

beforeEach(() => window.localStorage.clear());

describe('the annotator', () => {
	it('is the signed-in account, whatever the browser kept', async () => {
		window.localStorage.setItem('tracepad.annotator', 'somebody else');
		const { auth, annotator } = await fresh();
		auth.adopt(me('Ada') as never);

		expect(annotator.fromAccount).toBe(true);
		expect(annotator.name).toBe('Ada');
	});

	it('is the email of an account with no name', async () => {
		const { auth, annotator } = await fresh();
		auth.adopt(me('') as never);

		expect(annotator.name).toBe('ada@example.com');
	});

	it('is the name kept in the browser when nobody is signed in', async () => {
		const { annotator } = await fresh();
		expect(annotator.fromAccount).toBe(false);
		expect(annotator.name).toBeNull();

		expect(annotator.adopt('  bob ')).toBe(true);
		expect(annotator.name).toBe('bob');
	});
});
