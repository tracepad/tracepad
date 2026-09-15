import { beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '$lib/api/client.svelte';
import { auth } from './auth.svelte';
import { dashboard, setDashboard } from './preferences.svelte';

// The dashboard's arrangement, through the account (spec 034 #9): read from
// `me`, written whole, and a write the server refuses leaves the screen's
// arrangement alone and says why.

const patchMe = vi.fn();
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class ApiError extends Error {
		constructor(_status: number, message: string) {
			super(message);
		}
	},
	api: { patchMe: (...args: unknown[]) => patchMe(...args) }
}));

const P = 'a'.repeat(32);
const account = (preferences: Record<string, unknown>) => ({
	id: 'acc1',
	email: 'her@example.com',
	name: '',
	owner: false,
	preferences
});

/** A session whose account keeps these preferences. */
function signedIn(preferences: Record<string, unknown>) {
	auth.adopt({ account: account(preferences), projects: [] });
}

beforeEach(() => patchMe.mockClear());

describe('dashboard', () => {
	it('reads the arrangement out of me, made whole', () => {
		signedIn({ dashboard: { [P]: { order: ['errors', 'nonsense'], hidden: ['tokens'] } } });

		const { order, hidden } = dashboard(P);
		expect(order[0]).toBe('errors');
		expect(order).toHaveLength(10);
		expect(order).not.toContain('nonsense');
		expect(hidden).toEqual(['tokens']);
	});
});

describe('setDashboard', () => {
	it('writes the whole object back and takes the account the server answers with', async () => {
		signedIn({ theme: 'dark' });
		const written = { theme: 'dark', dashboard: { [P]: { order: ['errors'], hidden: [] } } };
		patchMe.mockResolvedValue({ account: account(written) });

		const failure = await setDashboard(P, { order: ['errors'], hidden: [] });

		expect(failure).toBeNull();
		expect(patchMe).toHaveBeenCalledWith({ preferences: written });
		expect(auth.account?.preferences).toEqual(written);
		expect(dashboard(P).order[0]).toBe('errors');
	});

	it('deletes the project key on reset', async () => {
		signedIn({ dashboard: { [P]: { order: ['errors'] }, other: {} } });
		patchMe.mockResolvedValue({ account: account({}) });

		await setDashboard(P, null);

		expect(patchMe).toHaveBeenCalledWith({ preferences: { dashboard: { other: {} } } });
	});

	it('keeps the state and surfaces the message when the write fails', async () => {
		signedIn({});
		// `Once`: a mock that keeps rejecting after a clear is reported by
		// vitest 4 as an unhandled rejection, though the code catches it.
		patchMe.mockRejectedValueOnce(new ApiError(422, 'preferences: larger than 16 KiB'));

		const failure = await setDashboard(P, { order: ['errors'], hidden: [] });

		expect(failure).toBe('preferences: larger than 16 KiB');
		expect(patchMe).toHaveBeenCalledOnce();
		expect(auth.account?.preferences).toEqual({});
	});

	// Anything that is not the server's own message — a network failure —
	// is one generic sentence.
	it("says one sentence for a failure that is not the server's", async () => {
		signedIn({});
		patchMe.mockRejectedValueOnce(new TypeError('network'));

		await expect(setDashboard(P, null)).resolves.toBe('The arrangement could not be saved.');
		expect(patchMe).toHaveBeenCalledOnce();
	});
});
