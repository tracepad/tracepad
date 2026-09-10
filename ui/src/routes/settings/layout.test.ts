import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { createRawSnippet } from 'svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import Layout from './+layout.svelte';

// Settings is three tabs because it is three audiences (spec 028 #14). The
// Server tab is an owner's and is *absent* for everybody else rather than
// disabled: there is nothing on it a member could act on, and the route
// redirects for anybody who types the address.

const url = { current: new URL('http://tracepad.test/settings/project') };
const goto = vi.fn();
const session = { owner: false };

vi.mock('$app/navigation', () => ({ goto: (...args: unknown[]) => goto(...args) }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return url.current;
		}
	}
}));
vi.mock('$lib/auth.svelte', () => ({
	get auth() {
		return session;
	}
}));

// The panel's content is the child route, which the router supplies and this
// test does not care about: an empty snippet stands in for it.
const children = createRawSnippet(() => ({ render: () => '<div></div>' }));

beforeEach(() => {
	url.current = new URL('http://tracepad.test/settings/project');
	goto.mockClear();
	session.owner = false;
});

describe('the tabs', () => {
	it('are Project and Account for a member', () => {
		render(Layout, { props: { children } });

		expect(screen.getAllByRole('tab').map((tab) => tab.textContent?.trim())).toEqual([
			'Project',
			'Account'
		]);
	});

	it('add Server for an owner', () => {
		session.owner = true;
		render(Layout, { props: { children } });

		expect(screen.getAllByRole('tab').map((tab) => tab.textContent?.trim())).toEqual([
			'Project',
			'Account',
			'Server'
		]);
	});

	it('read the active one out of the address', () => {
		url.current = new URL('http://tracepad.test/settings/account');
		render(Layout, { props: { children } });

		expect(screen.getByRole('tab', { name: 'Account' })).toHaveAttribute(
			'data-state',
			'active'
		);
	});

	// A tab is a link somebody can send, so picking one is a navigation and
	// not a piece of component state.
	it('navigate rather than switching in place', async () => {
		const person = userEvent.setup();
		render(Layout, { props: { children } });

		await person.click(screen.getByRole('tab', { name: 'Account' }));

		expect(goto).toHaveBeenCalledWith('/settings/account');
	});
});
