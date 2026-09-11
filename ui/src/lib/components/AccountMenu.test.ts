import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import AccountMenu from './AccountMenu.svelte';

// The account menu at the bottom of the sidebar (spec 028 #14): who is signed
// in, the role of the project on screen as a caption, and the two things the
// menu is for — the Account tab and signing out.

const goto = vi.fn();
const end = vi.fn();
const session = { displayName: 'Ada Lovelace', account: { email: 'ada@example.com' } };
const current = { role: 'viewer' as string | null };

vi.mock('$app/navigation', () => ({ goto: (...args: unknown[]) => goto(...args) }));
vi.mock('$lib/auth.svelte', () => ({
	get auth() {
		return session;
	},
	LOGIN_ROUTE: '/login'
}));
vi.mock('$lib/project.svelte', () => ({
	project: {
		get role() {
			return current.role;
		}
	}
}));
vi.mock('$lib/session', () => ({ end: () => end() }));

beforeEach(() => {
	goto.mockClear();
	end.mockReset();
	end.mockResolvedValue(undefined);
	session.displayName = 'Ada Lovelace';
	current.role = 'viewer';
});

describe('the trigger', () => {
	it('names the account and captions it with the role here', () => {
		render(AccountMenu);

		const trigger = screen.getByRole('button');
		expect(trigger).toHaveTextContent('Ada Lovelace');
		expect(trigger).toHaveTextContent('viewer');
		// The accessible name is stated rather than assembled from those two:
		// "editor" inside it would match every `name: 'Edit'` in the window.
		expect(trigger).toHaveAccessibleName('Signed in as Ada Lovelace');
	});

	// An account with no project has no role to caption, and an empty line
	// under the name would read as a missing value rather than as an absent
	// one.
	it('says nothing about a role when there is no project', () => {
		current.role = null;
		render(AccountMenu);

		expect(screen.getByRole('button')).not.toHaveTextContent('viewer');
	});
});

/**
 * Opens the menu and finds one of its items.
 *
 * `hidden` and `pointerEventsCheck` are both about jsdom rather than about the
 * menu: a floating wrapper positioned by a library that needs a layout engine
 * stays `visibility: hidden` where there is none, and bits-ui takes pointer
 * events off the page behind an open menu. In a browser neither is true, and
 * the Playwright suite drives this same menu for real.
 */
async function open(name: string) {
	const person = userEvent.setup({ pointerEventsCheck: 0 });
	render(AccountMenu);
	await person.click(screen.getByRole('button'));
	const items = await screen.findAllByRole('menuitem', { hidden: true });
	const item = items.find((one) => one.textContent?.trim() === name);
	if (!item) throw new Error(`no ${name} in ${items.map((one) => one.textContent?.trim())}`);
	return { person, items, item };
}

describe('the menu', () => {
	it('offers the account tab and signing out', async () => {
		const { items, item } = await open('Account');

		// Bare: the tab is about the person, not the project on screen (spec 029 #14).
		expect(item).toHaveAttribute('href', '/settings/account');
		expect(items.map((one) => one.textContent?.trim())).toEqual(['Account', 'Sign out']);
		// The email is here rather than on the trigger: it is what you check,
		// not what you read at a glance.
		expect(screen.getByText('ada@example.com')).toBeInTheDocument();
	});

	it('ends the session and leaves for the login form', async () => {
		const { person, item } = await open('Sign out');

		await person.click(item);

		await waitFor(() => expect(end).toHaveBeenCalled());
		expect(goto).toHaveBeenCalledWith('/login', { replaceState: true });
	});
});
