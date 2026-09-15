import { render, screen } from '@testing-library/svelte';
import { createRawSnippet, flushSync } from 'svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { auth } from '$lib/auth.svelte';
import { switcher } from '$lib/project.svelte';
import Layout from './+layout.svelte';

// The not-there screen (spec 029 #4) is decided when the URL names a project,
// not whenever `me` changes under it: a project deleted from the Server tab
// stays on screen — with the Restore that undoes it — until the next
// navigation, which is where the edge case in the spec puts it.

const P1 = 'a'.repeat(32);
const P2 = 'b'.repeat(32);

// Reactive, and replaced whole on a navigation the way SvelteKit replaces
// `page.params` — even one that lands on the same project.
const route = $state({ params: { project: P1 } });

vi.mock('$app/state', () => ({
	page: {
		get params() {
			return route.params;
		}
	}
}));

const children = createRawSnippet(() => ({ render: () => '<div>inside</div>' }));

function reaching(...ids: string[]) {
	auth.adopt({
		account: { id: 'acc1', email: 'her@example.com', name: '', owner: true, preferences: {} },
		projects: ids.map((id) => ({ id, name: id.slice(0, 4), role: 'owner' as const }))
	});
}

beforeEach(() => {
	route.params = { project: P1 };
	switcher.open = false;
});

describe('the project layout', () => {
	it('renders the screen for a project the account reaches', () => {
		reaching(P1);
		render(Layout, { props: { children } });

		expect(screen.getByText('inside')).toBeInTheDocument();
		expect(screen.queryByText('This project is not yours to see')).toBeNull();
		expect(switcher.open).toBe(false);
	});

	it('renders the not-there screen, with the switcher open, for one it does not', () => {
		reaching(P2);
		render(Layout, { props: { children } });

		expect(screen.getByText('This project is not yours to see')).toBeInTheDocument();
		expect(screen.getByText(P1)).toBeInTheDocument();
		expect(screen.queryByText('inside')).toBeNull();
		expect(switcher.open).toBe(true);
	});

	it('keeps the screen when `me` drops the project, until the next navigation', () => {
		reaching(P1, P2);
		render(Layout, { props: { children } });

		reaching(P2);
		flushSync();
		expect(screen.getByText('inside')).toBeInTheDocument();
		expect(switcher.open).toBe(false);

		route.params = { project: P1 };
		flushSync();
		expect(screen.getByText('This project is not yours to see')).toBeInTheDocument();
		expect(switcher.open).toBe(true);
	});

	// Closing the menu and following a sidebar link — same bad id, another
	// section — is another not-there screen, and the menu is open on it too.
	it('opens the switcher again on the next not-there navigation', () => {
		reaching(P2);
		render(Layout, { props: { children } });
		expect(switcher.open).toBe(true);

		switcher.open = false;
		route.params = { project: P1 };
		flushSync();
		expect(switcher.open).toBe(true);
	});
});
