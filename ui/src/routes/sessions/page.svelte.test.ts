import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import Page from './+page.svelte';

// The Sessions listing's environment control (spec 027 #8): a checkbox list of
// what there is, committed into the URL. `goto` is asynchronous, so what the
// boxes show while one is in flight is the thing under test here (#16).

// Reactive, so that a URL moving under an open panel is the move the page
// sees — which is what the last case here needs.
const url = $state({ current: new URL('http://tracepad.test/sessions') });
// Deliberately inert: it records the href and leaves the URL where it was,
// which is exactly the window between a click and the navigation landing.
const goto = vi.fn();
const listSessions = vi.fn();
const getFacets = vi.fn();

vi.mock('$app/navigation', () => ({
	goto: (href: string, opts?: unknown) => goto(href, opts)
}));
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
		listSessions: (...args: unknown[]) => listSessions(...args),
		getFacets: (...args: unknown[]) => getFacets(...args)
	}
}));

beforeEach(() => {
	url.current = new URL('http://tracepad.test/sessions');
	goto.mockClear();
	listSessions.mockReset();
	listSessions.mockResolvedValue({
		sessions: [],
		next_cursor: null,
		prev_cursor: null,
		total: 0,
		total_capped: false
	});
	getFacets.mockReset();
	getFacets.mockResolvedValue({
		from: '2026-09-01T00:00:00Z',
		to: '2026-09-09T00:00:00Z',
		environment: [
			{ value: 'production', count: 40 },
			{ value: 'staging', count: 12 },
			{ value: 'canary', count: 3 }
		],
		release: [],
		name: [],
		omitted: { environment: 0, release: 0, name: 0 }
	});
});

// The box for one value. By attribute rather than by role and name, because
// the popover is placed by floating-ui: with no layout to measure, jsdom
// leaves the content `visibility: hidden`, and a hidden node has no accessible
// name to match on. The `aria-label` is the string the field puts there.
function box(value: string): HTMLInputElement {
	const found = document.querySelector<HTMLInputElement>(`input[aria-label^="${value}"]`);
	if (!found) throw new Error(`no checkbox for ${value}`);
	return found;
}

/** Opens the environment popover and waits for the list to arrive. */
async function openEnvironments(user: ReturnType<typeof userEvent.setup>) {
	render(Page);
	await user.click(screen.getByRole('button', { name: /Environment/ }));
	await waitFor(() => box('production'));
}

describe('the environment control', () => {
	it('puts a ticked value in the URL as a list', async () => {
		const user = userEvent.setup();
		await openEnvironments(user);

		await user.click(box('production'));
		expect(goto).toHaveBeenCalledWith(
			expect.stringContaining('environment=production'),
			expect.anything()
		);
	});

	// The regression: `goto` has not landed, so the URL still says nothing.
	// Composing the second tick against the URL would write `staging` alone
	// and undo the first click; composing it against what the boxes show
	// carries both.
	it('carries the first tick into the second while the navigation is in flight', async () => {
		const user = userEvent.setup();
		await openEnvironments(user);

		await user.click(box('production'));
		await user.click(box('staging'));

		expect(goto).toHaveBeenCalledTimes(2);
		const href = new URL(goto.mock.calls[1][0] as string, 'http://tracepad.test');
		expect(href.searchParams.get('environment')).toBe('production,staging');
		expect(box('production').checked).toBe(true);
		expect(box('staging').checked).toBe(true);
	});

	// And it owns nothing beyond that gap: the URL is still the source of
	// truth, so a move it did not make — a link, the back button, Clear —
	// drops the pending list rather than fighting it.
	it('drops what it is holding when the URL moves', async () => {
		const user = userEvent.setup();
		await openEnvironments(user);

		await user.click(box('production'));
		expect(box('production').checked).toBe(true);

		url.current = new URL('http://tracepad.test/sessions?environment=canary');
		await waitFor(() => expect(box('canary').checked).toBe(true));
		expect(box('production').checked).toBe(false);
	});
});
