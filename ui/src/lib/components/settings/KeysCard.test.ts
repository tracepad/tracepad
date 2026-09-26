import { render, screen, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import KeysCard from './KeysCard.svelte';

// The Keys card (spec 045 #14): each row says which program holds the key, who
// minted it and whether it is still in use, and a key whose minter can no
// longer manage keys here says so on the row — nothing revoked it when they
// lost access (#10).

vi.mock('$app/state', () => ({ page: { url: new URL('http://tracepad.test/p/p1/settings') } }));

const KEYS = [
	{
		public_key: 'tp-pk-startup',
		name: '',
		scopes: ['ingest', 'read', 'write'],
		created_at: '2026-09-01T00:00:00Z',
		created_by: { kind: 'startup' },
		last_used_at: '2026-09-20T10:00:00Z'
	},
	{
		public_key: 'tp-pk-editor',
		name: 'checkout api',
		scopes: ['ingest', 'read', 'write'],
		created_at: '2026-09-02T00:00:00Z',
		created_by: { kind: 'account', account_id: 'a1', email: 'ed@example.com', standing: 'editor' },
		last_used_at: null
	},
	{
		public_key: 'tp-pk-gone',
		name: 'nightly eval',
		scopes: ['ingest', 'read', 'write'],
		created_at: '2026-09-03T00:00:00Z',
		created_by: { kind: 'account', email: 'left@example.com', standing: 'deleted' },
		last_used_at: null
	}
];

const listKeys = vi.fn(async () => ({ keys: KEYS }));
const createKey = vi.fn(async () => ({ public_key: 'tp-pk-new', secret_key: 'tp-sk-new' }));

vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: {
		listKeys: (...args: unknown[]) => listKeys(...(args as [])),
		createKey: (...args: unknown[]) => createKey(...(args as [])),
		revokeKey: vi.fn()
	}
}));

const PROJECT = { id: 'p1', name: 'checkout' };

beforeEach(() => {
	listKeys.mockClear();
	createKey.mockClear();
});

describe('the keys card', () => {
	it('says who minted each key and when it was last used', async () => {
		render(KeysCard, { current: PROJECT } as never);

		const editor = (await screen.findByText('checkout api')).closest('tr')!;
		expect(within(editor).getByText('by ed@example.com (editor)')).toBeTruthy();
		expect(within(editor).getByText('never')).toBeTruthy();
		expect(within(editor).queryByText(/can no longer manage keys/)).toBeNull();

		const server = screen.getByText('tp-pk-startup').closest('tr')!;
		expect(within(server).getByText('by the server')).toBeTruthy();
		expect(within(server).queryByText('never')).toBeNull();
	});

	it('marks a key whose minter can no longer manage keys here', async () => {
		render(KeysCard, { current: PROJECT } as never);

		const gone = (await screen.findByText('nightly eval')).closest('tr')!;
		expect(within(gone).getByText('by left@example.com (deleted)')).toBeTruthy();
		expect(within(gone).getByText(/can no longer manage keys here/)).toBeTruthy();
	});

	it('mints a key under the name typed, trimmed', async () => {
		render(KeysCard, { current: PROJECT } as never);
		const person = userEvent.setup({ pointerEventsCheck: 0 });

		await person.type(await screen.findByLabelText('Which program will hold the new key'), '  billing worker ');
		await person.click(screen.getByRole('button', { name: 'Mint a key pair' }));

		expect(createKey).toHaveBeenCalledWith('p1', 'billing worker');
	});

	it('shows a viewer neither the keys nor the form', () => {
		render(KeysCard, { current: PROJECT, readOnly: true } as never);

		expect(listKeys).not.toHaveBeenCalled();
		expect(screen.queryByLabelText('Which program will hold the new key')).toBeNull();
	});
});
