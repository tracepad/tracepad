import { render, screen, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { timestamp } from '$lib/format';
import { boxWidth } from '../../../tests/box';
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
const createKey = vi.fn(async (_id: string, scopes: string[]) => ({
	public_key: 'tp-pk-new',
	secret_key: 'tp-sk-new',
	scopes
}));

vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: {
		listKeys: (...args: unknown[]) => listKeys(...(args as [])),
		createKey: (...args: unknown[]) => createKey(...(args as [string, string[]])),
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
		expect(within(editor).getByText('ingest, read, write')).toBeTruthy();
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

	it('mints an ingest key under the name typed, trimmed, unless told otherwise', async () => {
		render(KeysCard, { current: PROJECT } as never);
		const person = userEvent.setup({ pointerEventsCheck: 0 });

		await person.type(await screen.findByLabelText('Which program will hold the new key'), '  billing worker ');
		expect(screen.getByRole('checkbox', { name: /^ingest/ })).toBeChecked();
		expect(screen.getByRole('checkbox', { name: /^read/ })).not.toBeChecked();
		expect(screen.getByRole('checkbox', { name: /^write/ })).not.toBeChecked();
		await person.click(screen.getByRole('button', { name: 'Mint a key pair' }));

		expect(createKey).toHaveBeenCalledWith('p1', ['ingest'], 'billing worker');
		// The dialog that follows fits the key: an exporter's lines, since it ingests.
		expect(await screen.findByText(/OTEL_EXPORTER_OTLP_HEADERS/)).toBeTruthy();
	});

	it('sends the scopes that are checked, in the server order, and none is not a key', async () => {
		render(KeysCard, { current: PROJECT } as never);
		const person = userEvent.setup({ pointerEventsCheck: 0 });
		const mint = await screen.findByRole('button', { name: 'Mint a key pair' });

		await person.click(screen.getByRole('checkbox', { name: /^ingest/ }));
		expect(mint).toBeDisabled();

		await person.click(screen.getByRole('checkbox', { name: /^write/ }));
		await person.click(screen.getByRole('checkbox', { name: /^read/ }));
		await person.click(mint);

		expect(createKey).toHaveBeenCalledWith('p1', ['read', 'write'], '');
		// A key that cannot ingest is not offered an exporter's headers.
		expect(await screen.findByText(/TRACEPAD_API_KEY=tp-sk-new/)).toBeTruthy();
		expect(screen.queryByText(/OTEL_EXPORTER_OTLP/)).toBeNull();
	});

	it('says what each scope lets a key do', async () => {
		render(KeysCard, { current: PROJECT } as never);
		const line = (scope: RegExp) =>
			screen.getByRole('checkbox', { name: scope }).closest('label')?.textContent;

		await screen.findByRole('checkbox', { name: /^read/ });
		expect(line(/^ingest/)).toContain('send spans');
		expect(line(/^read/)).toContain("every read of the project's data, and nothing that changes it");
		expect(line(/^write/)).toContain('deleting traces');
	});

	it('counts a name the way the server does, in characters', async () => {
		render(KeysCard, { current: PROJECT } as never);
		const person = userEvent.setup({ pointerEventsCheck: 0 });
		const field = await screen.findByLabelText('Which program will hold the new key');
		const mint = screen.getByRole('button', { name: 'Mint a key pair' });

		// Sixty-four emoji are 128 UTF-16 units and 64 characters: they fit.
		await person.click(field);
		await person.paste('🔑'.repeat(64));
		expect(mint.hasAttribute('disabled')).toBe(false);
		expect(screen.queryByRole('alert')).toBeNull();

		await person.paste('🔑');
		expect(mint.hasAttribute('disabled')).toBe(true);
		expect(screen.getByRole('alert').textContent).toContain('at most 64 characters');
	});

	it('shows a viewer neither the keys nor the form', () => {
		render(KeysCard, { current: PROJECT, readOnly: true } as never);

		expect(listKeys).not.toHaveBeenCalled();
		expect(screen.queryByLabelText('Which program will hold the new key')).toBeNull();
	});

	// In a box narrower than the table (spec 006 #24) the row is the key and
	// Revoke, and the rest folds under the public key.
	describe('in a narrow box', () => {
		it('folds the scopes, the minting and the last use under the key, and names Revoke by it', async () => {
			boxWidth(322);
			render(KeysCard, { current: PROJECT } as never);

			const editor = (await screen.findByText('tp-pk-editor')).closest('tr')!;
			expect(screen.getAllByRole('columnheader').map((one) => one.textContent?.trim())).toEqual(['Name', 'Actions']);
			expect(editor).toHaveTextContent(
				`ingest, read, write · created ${timestamp('2026-09-02T00:00:00Z')} · by ed@example.com (editor) · last used never`
			);
			expect(within(editor).getByRole('button', { name: 'Revoke tp-pk-editor' })).toBeTruthy();

			const gone = screen.getByText('tp-pk-gone').closest('tr')!;
			expect(within(gone).getByText(/can no longer manage keys here/)).toBeTruthy();
		});

		it('is the whole table in a box as wide as it', async () => {
			boxWidth(640);
			render(KeysCard, { current: PROJECT } as never);

			await screen.findByText('tp-pk-editor');
			expect(screen.getAllByRole('columnheader')).toHaveLength(5);
			expect(screen.getAllByRole('button', { name: 'Revoke' })).toHaveLength(3);
		});
	});
});
