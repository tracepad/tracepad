import { render, screen } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { auth } from '$lib/auth.svelte';
import Page from './+page.svelte';

// The Traces empty state (spec 034 #6): with nothing filtered and nothing
// listed, one line and a pointer to the dashboard, where the setup is.

const PROJECT = vi.hoisted(() => 'a'.repeat(32));
const url = $state({ current: new URL(`http://tracepad.test/p/${PROJECT}/traces`) });
const listTraces = vi.fn();

vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/state', () => ({
	page: {
		get url() {
			return url.current;
		},
		get params() {
			return { project: PROJECT };
		}
	}
}));
vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: {
		listTraces: (...args: unknown[]) => listTraces(...args),
		getFacets: () => new Promise(() => {})
	}
}));

beforeEach(() => {
	url.current = new URL(`http://tracepad.test/p/${PROJECT}/traces`);
	listTraces.mockReset();
	listTraces.mockResolvedValue({ traces: [], next_cursor: null, prev_cursor: null, total: 0 });
	auth.adopt({
		account: { id: 'acc1', email: 'her@example.com', name: '', owner: false, preferences: {} },
		projects: [{ id: PROJECT, name: 'demo', role: 'viewer' }]
	});
});

describe('the empty state', () => {
	it('points at the dashboard when nothing is filtered', async () => {
		render(Page);

		expect(await screen.findByRole('heading', { name: /No traces yet/ })).toBeInTheDocument();
		expect(screen.getByRole('link', { name: 'dashboard' })).toHaveAttribute(
			'href',
			`/p/${PROJECT}/dashboard`
		);
		// The exporter settings live on the dashboard now, not here.
		expect(screen.queryByText(/OTEL_EXPORTER_OTLP_TRACES_ENDPOINT/)).toBeNull();
	});

	it('says nothing matches when a filter is set', async () => {
		url.current = new URL(`http://tracepad.test/p/${PROJECT}/traces?status=error`);
		render(Page);

		expect(await screen.findByRole('heading', { name: /No trace matches/ })).toBeInTheDocument();
		expect(screen.queryByRole('link', { name: 'dashboard' })).toBeNull();
	});
});
