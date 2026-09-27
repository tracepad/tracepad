import { render, screen } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import Setup from './+page.svelte';

// The setup screen says what it can: the form while the server takes a setup
// link, and what to do instead when it runs with TRACEPAD_SETUP=off (spec 028
// #32) — not "this link carries no token", which sends the reader looking for
// a link that will never be printed.

const getSetup = vi.fn();
let token: string | null = 'a-setup-token';

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$lib/session', () => ({ begin: vi.fn() }));
vi.mock('$lib/project.svelte', () => ({ bareTarget: (path: string) => path }));
vi.mock('$lib/auth.svelte', () => ({
	LOGIN_ROUTE: '/login',
	auth: { tokenFromFragment: () => token, stripFragment: vi.fn() }
}));
vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: { getSetup: () => getSetup(), setup: vi.fn() }
}));

beforeEach(() => {
	getSetup.mockReset();
	token = 'a-setup-token';
});

describe('the setup screen', () => {
	it('asks for the first owner while setup is on', async () => {
		getSetup.mockResolvedValue({ required: true, enabled: true, expired: false });
		render(Setup);

		expect(await screen.findByRole('button', { name: 'Create the owner' })).toBeTruthy();
	});

	it('says setup is off, and how the first owner is made instead', async () => {
		getSetup.mockResolvedValue({ required: true, enabled: false, expired: false });
		token = null;
		render(Setup);

		const text = await screen.findByText(/Setup is turned off on this server/);
		// A command that runs as printed: the CLI's credential is
		// TRACEPAD_API_KEY, and here that is the admin token.
		expect(text.textContent).toContain('TRACEPAD_API_KEY=$TRACEPAD_ADMIN_TOKEN tracepad accounts create');
		expect(screen.queryByText(/carries no setup token/)).toBeNull();
		expect(screen.queryByRole('button', { name: 'Create the owner' })).toBeNull();
	});

	// Off, with an owner already: there is nothing to set up, and nothing
	// about a link the server never prints.
	it('with setup off and an owner, says there is nothing to set up', async () => {
		getSetup.mockResolvedValue({ required: false, enabled: false, expired: false });
		token = null;
		render(Setup);

		expect(await screen.findByText(/already has an owner/)).toBeTruthy();
		expect(screen.queryByText(/carries no setup token/)).toBeNull();
	});

	it('says no link works any more before anybody fills in the form', async () => {
		getSetup.mockResolvedValue({ required: true, enabled: true, expired: true });
		render(Setup);

		expect(await screen.findByText(/No setup link from this start/)).toBeTruthy();
		expect(screen.queryByRole('button', { name: 'Create the owner' })).toBeNull();
	});
});
