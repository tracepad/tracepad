import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import Page from './+page.svelte';

// The Users listing's controls (spec 023 #8): the sort select and the prefix
// box put their state in the URL, because the view is a link. The rows
// themselves are the endpoint's, and `UserTable` renders them.

const url = { current: new URL('http://tracepad.test/users') };
const goto = vi.fn((href: string) => {
	url.current = new URL(href, 'http://tracepad.test');
});
const listUsers = vi.fn();

vi.mock('$app/navigation', () => ({ goto: (href: string) => goto(href) }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return url.current;
		}
	}
}));
vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: {
		listUsers: (...args: unknown[]) => listUsers(...args)
	}
}));

const page = (users: unknown[]) => ({
	users,
	next_cursor: null,
	prev_cursor: null,
	total: users.length,
	total_capped: false
});

beforeEach(() => {
	url.current = new URL('http://tracepad.test/users');
	goto.mockClear();
	listUsers.mockReset();
	listUsers.mockResolvedValue(
		page([
			{ user_id: 'alice', traces: 3, error_count: 1, total_cost: 0.5, sessions: 2 },
			{ user_id: 'bob', traces: 1, error_count: 0, sessions: 1 }
		])
	);
});

describe('the filter bar', () => {
	it('renders the rows the endpoint returned', async () => {
		render(Page);

		await waitFor(() => expect(screen.getByText('alice')).toBeInTheDocument());
		expect(screen.getByText('bob')).toBeInTheDocument();
	});

	it('writes the sort into the URL', async () => {
		render(Page);
		await waitFor(() => expect(listUsers).toHaveBeenCalled());

		await userEvent.selectOptions(screen.getByLabelText('Sort by'), 'cost');

		expect(goto).toHaveBeenCalledWith('/users?sort=cost');
	});

	it('writes the prefix into the URL', async () => {
		render(Page);
		await waitFor(() => expect(listUsers).toHaveBeenCalled());

		await userEvent.type(screen.getByLabelText('User id prefix'), 'acme:');
		// Committed on blur, never on every keystroke.
		expect(goto).not.toHaveBeenCalled();
		await userEvent.tab();

		expect(goto).toHaveBeenCalledWith('/users?prefix=acme%3A');
	});

	it('clears the prefix when the box is emptied', async () => {
		url.current = new URL('http://tracepad.test/users?prefix=acme%3A');
		render(Page);
		await waitFor(() => expect(listUsers).toHaveBeenCalled());

		await userEvent.clear(screen.getByLabelText('User id prefix'));
		await userEvent.tab();

		expect(goto).toHaveBeenCalledWith('/users');
	});

	it('asks the endpoint for what the URL says', async () => {
		url.current = new URL('http://tracepad.test/users?sort=traces&prefix=a');
		render(Page);

		await waitFor(() => expect(listUsers).toHaveBeenCalled());
		expect(listUsers.mock.calls[0][0]).toEqual({ sort: 'traces', prefix: 'a' });
	});
});

describe('the empty state', () => {
	it('names the lag rather than claiming there are no users', async () => {
		listUsers.mockResolvedValue(page([]));
		render(Page);

		await waitFor(() =>
			expect(screen.getByText('No user has been seen yet')).toBeInTheDocument()
		);
		// The edge case spec 023 names: the listing is the roll-up, and the
		// traces behind it are somewhere the reader can go.
		expect(screen.getByText(/trails live traffic/)).toBeInTheDocument();
		expect(screen.getByRole('link', { name: 'Traces' })).toHaveAttribute('href', '/traces');
	});
});
