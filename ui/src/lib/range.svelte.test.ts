import { beforeEach, describe, expect, it, vi } from 'vitest';

// The window remembered in this browser (spec 034 #7): a preset as a preset,
// re-resolved against the clock; a calendar range as its days; per account;
// and nothing that could break a screen when the value is not what it should
// be.

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));

const NOW = new Date('2026-09-15T12:00:00Z');
const LATER = new Date('2026-09-22T12:00:00Z');

async function fresh(accountId: string | null) {
	vi.resetModules();
	const { auth } = await import('./auth.svelte');
	if (accountId) {
		auth.adopt({
			account: { id: accountId, email: 'her@example.com', name: '', owner: false, preferences: {} },
			projects: []
		});
	}
	return await import('./range.svelte');
}

beforeEach(() => window.localStorage.clear());

describe('rememberedRange', () => {
	it('is nothing until something was set', async () => {
		const { rememberedRange } = await fresh('acc1');
		expect(rememberedRange(NOW)).toBeNull();
	});

	it('re-resolves a preset against the clock it is asked for', async () => {
		const { rememberRange, rememberedRange } = await fresh('acc1');
		rememberRange({ from: new Date(NOW.getTime() - 86_400_000).toISOString() }, NOW);

		expect(window.localStorage.getItem('tracepad.range.acc1')).toBe('{"preset":"24h"}');
		expect(rememberedRange(LATER)).toEqual({
			from: new Date(LATER.getTime() - 86_400_000).toISOString()
		});
	});

	it('keeps a calendar range as its dates', async () => {
		const { rememberRange, rememberedRange } = await fresh('acc1');
		const days = { from: '2026-09-01T00:00:00.000Z', to: '2026-09-03T00:00:00.000Z' };
		rememberRange(days, NOW);

		expect(rememberedRange(LATER)).toEqual(days);
	});

	it('falls back on a value that is not a window', async () => {
		const { rememberedRange } = await fresh('acc1');
		for (const broken of ['nonsense', '[]', '7', '{"preset":"1y"}', '{"from":"yesterday"}', '{}']) {
			window.localStorage.setItem('tracepad.range.acc1', broken);
			expect(rememberedRange(NOW)).toBeNull();
		}
	});

	it('forgets the window on Any time', async () => {
		const { rememberRange, rememberedRange } = await fresh('acc1');
		rememberRange({ from: '2026-09-01T00:00:00.000Z' }, NOW);
		rememberRange({}, NOW);

		expect(rememberedRange(NOW)).toBeNull();
	});

	it('is per account, and nothing for nobody', async () => {
		const one = await fresh('acc1');
		one.rememberRange({ from: '2026-09-01T00:00:00.000Z' }, NOW);

		const other = await fresh('acc2');
		expect(other.rememberedRange(NOW)).toBeNull();

		const nobody = await fresh(null);
		expect(nobody.rememberedRange(NOW)).toBeNull();
		nobody.rememberRange({ from: '2026-09-01T00:00:00.000Z' }, NOW);
		expect(window.localStorage.length).toBe(1);
	});
});
