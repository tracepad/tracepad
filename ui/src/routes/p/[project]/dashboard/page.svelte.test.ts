import { render, screen, waitFor, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { auth } from '$lib/auth.svelte';
import { BLOCKS } from '$lib/dashboard';
import Page from './+page.svelte';

// The project's front page (spec 034): the blocks in the account's order,
// hidden ones absent and asking for nothing, the last-trace line, and a
// fresh project's instructions in place of the blocks.

// uPlot reads the pixel ratio through `matchMedia` as it loads, and jsdom has
// no such thing; the page reads `prefers-reduced-motion` the same way.
vi.hoisted(() => {
	window.matchMedia = () =>
		({ matches: false, addEventListener() {}, removeEventListener() {} }) as never;
	// `animate:flip` asks a moving element for its animations.
	Element.prototype.getAnimations = () => [];
});

const PROJECT = vi.hoisted(() => 'a'.repeat(32));
const AT = `http://tracepad.test/p/${PROJECT}/dashboard?from=2026-09-08T00:00:00Z`;
const url = $state({ current: new URL(AT) });
const goto = vi.fn();
const getStats = vi.fn();
const listTraces = vi.fn();
const getScoreTrends = vi.fn();
const listScoreConfigs = vi.fn();
const patchMe = vi.fn();

vi.mock('$app/navigation', () => ({ goto: (href: string, opts?: unknown) => goto(href, opts) }));
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
	ApiError: class extends Error {
		constructor(_status: number, message: string) {
			super(message);
		}
	},
	api: {
		getStats: (...args: unknown[]) => getStats(...args),
		listTraces: (...args: unknown[]) => listTraces(...args),
		getScoreTrends: (...args: unknown[]) => getScoreTrends(...args),
		listScoreConfigs: (...args: unknown[]) => listScoreConfigs(...args),
		patchMe: (...args: unknown[]) => patchMe(...args)
	}
}));

const NOW = new Date('2026-09-15T12:00:00Z');
const trace = { id: 'c'.repeat(32), name: 'chat', timestamp: '2026-09-15T11:56:00Z' };

/** A session whose account keeps this arrangement for the project. */
function signedIn(arrangement: Record<string, unknown> | null = null) {
	auth.adopt({
		account: {
			id: 'acc1',
			email: 'her@example.com',
			name: '',
			owner: false,
			preferences: arrangement ? { dashboard: { [PROJECT]: arrangement } } : {}
		},
		projects: [{ id: PROJECT, name: 'demo', role: 'viewer' }]
	});
}

beforeEach(() => {
	vi.useFakeTimers({ now: NOW, toFake: ['Date'] });
	url.current = new URL(AT);
	for (const mock of [goto, getStats, listTraces, getScoreTrends, listScoreConfigs, patchMe]) mock.mockClear();
	getStats.mockImplementation((query: { group_by: string }) =>
		Promise.resolve({
			group_by: query.group_by,
			unit: 'trace',
			buckets:
				query.group_by === 'total'
					? [{ key: '', count: 12, error_count: 1, total_cost: 0.5, latency_ms: { p50: 1, p95: 2 } }]
					: []
		})
	);
	listTraces.mockResolvedValue({ traces: [trace], next_cursor: null, prev_cursor: null });
	getScoreTrends.mockResolvedValue({ group_by: 'day', targets: 'all', omitted: 0, series: [] });
	listScoreConfigs.mockResolvedValue({ configs: [] });
	patchMe.mockImplementation((body: { preferences: unknown }) =>
		Promise.resolve({ account: { ...auth.account, preferences: body.preferences } })
	);
	signedIn();
});

/** The block labels on screen, in document order. */
const blocksShown = () =>
	[...document.querySelectorAll('[aria-label="Dashboard blocks"] > [aria-label]')].map((node) =>
		node.getAttribute('aria-label')
	);

const settled = () => waitFor(() => expect(listTraces).toHaveBeenCalled());

describe('the blocks', () => {
	it('render in the default order for an account that kept nothing', async () => {
		getScoreTrends.mockResolvedValue({
			group_by: 'day',
			targets: 'all',
			omitted: 0,
			series: [{ name: 'helpful', data_type: 'boolean', buckets: [] }]
		});
		render(Page);
		await settled();

		await waitFor(() => expect(blocksShown()).toEqual(BLOCKS.map((block) => block.label)));
		// The summary row is two `total` requests: this window and the one before it.
		const totals = getStats.mock.calls.filter(([query]) => query.group_by === 'total');
		expect(totals).toHaveLength(2);
		// Seven and a half days, ending where this window begins.
		expect(totals[1][0]).toMatchObject({ from: '2026-08-31T12:00:00.000Z', to: '2026-09-08T00:00:00.000Z' });
		await waitFor(() => expect(screen.getByText('12')).toBeInTheDocument());
	});

	it('render in the order given, without the hidden ones', async () => {
		signedIn({ order: ['errors', 'summary', 'quality'], hidden: ['tokens', 'models'] });
		render(Page);
		await settled();

		// No Quality block either: the window holds no score (Decision 5).
		expect(blocksShown()).toEqual([
			'Errors', 'Summary', 'Traces', 'Cost', 'Latency', 'By environment', 'By release'
		]);
		// A hidden breakdown asks for nothing.
		expect(getStats.mock.calls.map(([query]) => query.group_by)).not.toContain('model');
	});

	it('make no quality request while the block is hidden', async () => {
		signedIn({ order: [], hidden: ['quality'] });
		render(Page);
		await settled();
		await waitFor(() => expect(getStats).toHaveBeenCalled());

		expect(getScoreTrends).not.toHaveBeenCalled();
		expect(listScoreConfigs).not.toHaveBeenCalled();
	});

	it('skip the timeline when every chart is hidden', async () => {
		signedIn({ order: [], hidden: ['traces', 'cost', 'tokens', 'latency', 'errors'] });
		render(Page);
		await settled();
		await waitFor(() => expect(getStats).toHaveBeenCalled());

		expect(getStats.mock.calls.map(([query]) => query.group_by)).not.toContain('day');
	});
});

describe('the timeline size', () => {
	const timeline = () =>
		getStats.mock.calls.map(([query]) => query.group_by).filter((group) => ['minute', 'hour', 'day'].includes(group));

	it('reads the last hour by the minute, and the quality cards by the hour', async () => {
		url.current = new URL(`http://tracepad.test/p/${PROJECT}/dashboard?from=2026-09-15T11:00:00Z`);
		render(Page);
		await settled();
		await waitFor(() => expect(getScoreTrends).toHaveBeenCalled());

		expect(timeline()).toEqual(['minute']);
		expect(getScoreTrends.mock.calls[0][0]).toMatchObject({ group_by: 'hour' });
		expect(screen.getByRole('button', { name: 'Minutely' })).toHaveAttribute('aria-pressed', 'true');
	});

	it('offers no minutes over a week, whatever the link says', async () => {
		url.current = new URL(`${AT}&group_by=minute`);
		render(Page);
		await settled();

		expect(timeline()).toEqual(['day']);
		const minutely = screen.getByRole('button', { name: 'Minutely' });
		expect(minutely).toBeDisabled();
		expect(minutely).toHaveAttribute('title', expect.stringContaining('24 hours or less'));
	});
});

describe('the last-trace line', () => {
	it('says when the newest trace arrived, with the instant in the tooltip', async () => {
		render(Page);

		const line = await screen.findByText(/Last trace 4 minutes ago/);
		expect(line).toHaveAttribute('title');
		// Read without a window: the question is about the project, not the range.
		expect(listTraces.mock.calls[0][0]).toEqual({ environment: undefined });
		expect(listTraces.mock.calls[0][1]).toEqual({ limit: 1 });
	});

	it('keeps the blocks when only the environment filter finds nothing', async () => {
		url.current = new URL(`${AT}&environment=nowhere`);
		listTraces.mockResolvedValue({ traces: [], next_cursor: null, prev_cursor: null });
		render(Page);

		await waitFor(() => expect(screen.getByText('No traces yet')).toBeInTheDocument());
		expect(screen.queryByText(/OTEL_EXPORTER_OTLP_TRACES_ENDPOINT/)).toBeNull();
		expect(document.querySelector('[aria-label="Dashboard blocks"]')).not.toBeNull();
	});

	it('says no traces yet, and shows the onboarding card in place of the blocks', async () => {
		listTraces.mockResolvedValue({ traces: [], next_cursor: null, prev_cursor: null });
		render(Page);

		expect(await screen.findByRole('heading', { name: /No traces yet/ })).toBeInTheDocument();
		expect(screen.getByText(/OTEL_EXPORTER_OTLP_TRACES_ENDPOINT/)).toBeInTheDocument();
		expect(screen.getByRole('link', { name: 'Quickstart' })).toBeInTheDocument();
		expect(document.querySelector('[aria-label="Dashboard blocks"]')).toBeNull();
		expect(screen.queryByRole('button', { name: /Customize/ })).toBeNull();
	});
});

describe('Customize', () => {
	it('hides a block, writes the arrangement, and brings it back from the strip', async () => {
		const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
		render(Page);
		await settled();

		await user.click(screen.getByRole('button', { name: /Customize/ }));
		await user.click(screen.getByRole('button', { name: 'Hide Tokens' }));

		await waitFor(() => expect(blocksShown()).not.toContain('Tokens'));
		expect(patchMe).toHaveBeenCalledWith({
			preferences: {
				dashboard: { [PROJECT]: { order: BLOCKS.map((block) => block.id), hidden: ['tokens'] } }
			}
		});
		const strip = screen.getByLabelText('Hidden blocks');
		await user.click(within(strip).getByRole('button', { name: 'Show Tokens' }));
		await waitFor(() => expect(blocksShown()).toContain('Tokens'));

		// Reset deletes the project's key; Done leaves the mode.
		await user.click(screen.getByRole('button', { name: 'Reset' }));
		expect(patchMe).toHaveBeenLastCalledWith({ preferences: { dashboard: {} } });
		await user.click(screen.getByRole('button', { name: 'Done' }));
		expect(screen.queryByLabelText('Hidden blocks')).toBeNull();
	});

	it('keeps the arrangement on screen and shows the message when the write is refused', async () => {
		const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
		const { ApiError } = await import('$lib/api/client.svelte');
		patchMe.mockRejectedValueOnce(new ApiError(422, 'preferences: larger than 16 KiB'));
		render(Page);
		await settled();

		await user.click(screen.getByRole('button', { name: /Customize/ }));
		await user.click(screen.getByRole('button', { name: 'Hide Tokens' }));

		// The library's own alert region (for the drag announcements) is the
		// other one on the page.
		const strip = screen.getByLabelText('Hidden blocks');
		await waitFor(() =>
			expect(within(strip).getByRole('alert')).toHaveTextContent('preferences: larger than 16 KiB')
		);
		expect(blocksShown()).not.toContain('Tokens');
	});
});
