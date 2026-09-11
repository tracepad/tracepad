import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { createRawSnippet } from 'svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import SettingsTabs from './SettingsTabs.svelte';

// Settings is three tabs because it is three audiences (spec 028 #14). The
// Server tab is an owner's and is *absent* for everybody else rather than
// disabled: there is nothing on it a member could act on, and the route
// redirects for anybody who types the address.

// Project and Server live under the project on screen (spec 029 #1); Account
// is about the person and lives bare (#14). The strip reads which tab out of
// the address after the prefix, if there is one.
const PROJECT = 'a'.repeat(32);
const REMEMBERED = 'b'.repeat(32);
const url = { current: new URL(`http://tracepad.test/p/${PROJECT}/settings/project`) };
const params = { current: { project: PROJECT } as { project?: string } };
const goto = vi.fn();
const session = { owner: false };
const remembered = vi.fn<() => string | null>();

vi.mock('$app/navigation', () => ({ goto: (...args: unknown[]) => goto(...args) }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return url.current;
		},
		get params() {
			return params.current;
		}
	}
}));
vi.mock('$lib/auth.svelte', () => ({
	get auth() {
		return session;
	}
}));
vi.mock('$lib/project.svelte', async () => ({
	...(await vi.importActual<typeof import('$lib/project.svelte')>('$lib/project.svelte')),
	project: { remembered: () => remembered() }
}));

// The panel's content is the child route, which the router supplies and this
// test does not care about: an empty snippet stands in for it.
const children = createRawSnippet(() => ({ render: () => '<div></div>' }));

const tabs = () => screen.getAllByRole('tab').map((tab) => tab.textContent?.trim());

beforeEach(() => {
	url.current = new URL(`http://tracepad.test/p/${PROJECT}/settings/project`);
	params.current = { project: PROJECT };
	goto.mockClear();
	session.owner = false;
	remembered.mockReturnValue(null);
});

describe('the tabs', () => {
	it('are Project and Account for a member', () => {
		render(SettingsTabs, { props: { children } });

		expect(tabs()).toEqual(['Project', 'Account']);
	});

	it('add Server for an owner', () => {
		session.owner = true;
		render(SettingsTabs, { props: { children } });

		expect(tabs()).toEqual(['Project', 'Account', 'Server']);
	});

	it('read the active one out of the address', () => {
		url.current = new URL(`http://tracepad.test/settings/account`);
		params.current = {};
		render(SettingsTabs, { props: { children } });

		expect(screen.getByRole('tab', { name: 'Account' })).toHaveAttribute('data-state', 'active');
	});

	// A tab is a link somebody can send, so picking one is a navigation and
	// not a piece of component state. Account's address has no project in it.
	it('navigate rather than switching in place', async () => {
		const person = userEvent.setup();
		render(SettingsTabs, { props: { children } });

		await person.click(screen.getByRole('tab', { name: 'Account' }));

		expect(goto).toHaveBeenCalledWith('/settings/account');
	});
});

describe('on the bare Account tab', () => {
	beforeEach(() => {
		url.current = new URL(`http://tracepad.test/settings/account`);
		params.current = {};
	});

	it('the other tabs point at the remembered project', async () => {
		remembered.mockReturnValue(REMEMBERED);
		session.owner = true;
		const person = userEvent.setup();
		render(SettingsTabs, { props: { children } });

		expect(tabs()).toEqual(['Project', 'Account', 'Server']);
		await person.click(screen.getByRole('tab', { name: 'Server' }));
		expect(goto).toHaveBeenCalledWith(`/p/${REMEMBERED}/settings/server`);
	});

	it('is the only tab for an account that reaches no project', () => {
		session.owner = true;
		render(SettingsTabs, { props: { children } });

		expect(tabs()).toEqual(['Account']);
	});
});
