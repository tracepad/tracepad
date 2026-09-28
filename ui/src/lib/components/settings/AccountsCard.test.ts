import { render, screen, within } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import AccountsCard from './AccountsCard.svelte';
import { boxWidth } from '../../../tests/box';

// The Accounts card at the two widths (spec 006 #18). On a phone the row is
// the email and the two buttons, and what the other columns said folds under
// the email — named, because the heads that said what "never" and "none" are
// about are not on the screen to say it. The cell is not the row's header
// there: a header is read before every cell, and this one is the whole
// account, so the buttons carry the email in their names instead.

vi.mock('$app/state', () => ({ page: { url: new URL('http://tracepad.test/p/p1/settings') } }));

const ACCOUNTS = [
	{
		id: 'a1',
		email: 'ada@example.com',
		name: 'Ada',
		owner: false,
		disabled: false,
		pending: true,
		last_login_at: null,
		projects: []
	}
];

vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: {
		listAccounts: vi.fn(async () => ({ accounts: ACCOUNTS })),
		listAllProjects: vi.fn(async () => ({ projects: [] }))
	}
}));

let narrow = false;
beforeEach(() => {
	narrow = false;
	window.matchMedia = (query: string) =>
		({ matches: narrow, media: query, addEventListener() {}, removeEventListener() {} }) as never;
});

const heads = () => screen.getAllByRole('columnheader').map((one) => one.textContent?.trim());

describe('the accounts card', () => {
	it('has every column where there is room', async () => {
		render(AccountsCard);

		await screen.findByText('ada@example.com');
		expect(heads()).toEqual(['Email', 'Name', 'Status', 'Last login', 'Projects', 'Actions']);
	});

	it('folds the row under the email on a phone, and says what each part is', async () => {
		narrow = true;
		render(AccountsCard);

		const email = (await screen.findByText('ada@example.com')).closest('td')!;
		expect(heads()).toEqual(['Email', 'Actions']);
		expect(screen.queryByRole('rowheader')).not.toBeInTheDocument();
		expect(within(email).getByText('pending')).toHaveClass('text-warn');
		expect(email).toHaveTextContent('Ada · pending · last login never');
		expect(email).toHaveTextContent('Projects: none');
		const row = email.closest('tr')!;
		expect(within(row).getByRole('button', { name: 'Edit ada@example.com' })).toBeInTheDocument();
		expect(within(row).getByRole('button', { name: 'Delete ada@example.com' })).toBeInTheDocument();
	});
});

describe('the accounts card by its box', () => {
	it('folds in a box narrower than the table, on a wide screen (#22)', async () => {
		boxWidth(700);
		render(AccountsCard);

		await screen.findByText('ada@example.com');
		expect(heads()).toEqual(['Email', 'Actions']);
	});

	it('keeps every column in the card a desktop gives it', async () => {
		boxWidth(730);
		render(AccountsCard);

		await screen.findByText('ada@example.com');
		expect(heads()).toHaveLength(6);
	});
});
