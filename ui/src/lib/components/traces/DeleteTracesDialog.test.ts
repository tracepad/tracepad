import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import DeleteTracesDialog from './DeleteTracesDialog.svelte';

// *Delete…* on the traces listing (spec 035 #9): the dialog names the filters
// as chips and the listing's count, pins `to` to the moment it opened, asks
// for the project name, loops rounds with a progress line, and *Stop* finishes
// the round in flight and leaves the rest.

type Call = { filters: Record<string, unknown>; confirm?: string; limit?: number };

const { deleteTraces, calls, rounds } = vi.hoisted(() => {
	const calls: Call[] = [];
	const rounds = { left: 2500 };
	return {
		calls,
		rounds,
		deleteTraces: vi.fn(async (filters: Record<string, unknown>, confirm?: string, limit?: number) => {
			calls.push({ filters, confirm, limit });
			if (confirm === undefined) {
				return {
					dry_run: true as const,
					matched: rounds.left,
					would_delete: { traces: rounds.left, observations: rounds.left * 3, scores: 0, annotation_items: 0 },
					oldest: '2026-09-01T09:00:00Z',
					affected_runs: [],
					confirm: 'acme',
					note: 'raw OTLP bodies are not deleted; they expire on the raw retention window'
				};
			}
			const taken = Math.min(limit ?? 1000, rounds.left);
			rounds.left -= taken;
			return {
				dry_run: false as const,
				deleted: { traces: taken, observations: taken * 3, scores: 0, payloads: 0, annotation_items: 0 },
				more: rounds.left > 0
			};
		})
	};
});

vi.mock('$lib/api/client.svelte', () => ({ ApiError: class extends Error {}, api: { deleteTraces } }));

function dialog(filters: Record<string, unknown>, matched = '1,000+ traces') {
	const ondeleted = vi.fn();
	render(DeleteTracesDialog, {
		props: { open: true, filters, matched, onclose: vi.fn(), ondeleted }
	} as never);
	return { ondeleted, user: userEvent.setup({ delay: null }) };
}

beforeEach(() => {
	deleteTraces.mockClear();
	calls.length = 0;
	rounds.left = 2500;
	document.body.style.pointerEvents = '';
});

describe('the delete-by-filter dialog', () => {
	it('names the filters as chips, the count, and the moment it pinned', async () => {
		const before = Date.now();
		dialog({ environment: 'production,staging', status: 'error', q: 'refund' });

		expect(screen.getByText(/1,000\+ traces on the listing match/)).toBeInTheDocument();
		// The bar's own chips, not a prose rendering of the filter (#12).
		expect(screen.getByText('Environment: production, staging')).toBeInTheDocument();
		expect(screen.getByText('Status: error')).toBeInTheDocument();
		expect(screen.getByText('refund')).toBeInTheDocument();
		expect(screen.getByText(/the moment this dialog opened/)).toBeInTheDocument();

		// The dry run runs as the dialog opens: the count is what it is for.
		expect(await screen.findByText('2,500')).toBeInTheDocument();

		// `to` was filled in at the moment of opening and sent with the rest.
		const sent = calls[0].filters;
		expect(sent.environment).toBe('production,staging');
		expect(sent.q).toBe('refund');
		expect(typeof sent.to).toBe('string');
		expect(Date.parse(sent.to as string)).toBeGreaterThanOrEqual(before - 1000);
	});

	it('keeps a `to` the filter already has, and says so plainly with no filter', () => {
		dialog({ to: '2026-09-10T00:00:00Z' }, '412 traces');

		expect(screen.getByText(/no filter at all/)).toBeInTheDocument();
		expect(screen.queryByText(/the moment this dialog opened/)).not.toBeInTheDocument();
		expect(screen.getByText(/traces that started before/)).toBeInTheDocument();
	});

	it('loops rounds with the echo until there is no more, and reports the total', async () => {
		const { ondeleted, user } = dialog({ status: 'error' });
		await user.type(await screen.findByRole('textbox'), 'acme');
		await user.click(screen.getByRole('button', { name: 'Delete these traces' }));

		expect(await screen.findByText('Deleted 2,500 traces.')).toBeInTheDocument();
		// One preview, three rounds of a thousand, each with the same echo.
		const confirmed = calls.filter((call) => call.confirm !== undefined);
		expect(confirmed).toHaveLength(3);
		expect(confirmed.every((call) => call.confirm === 'acme' && call.limit === 1000)).toBe(true);
		expect(screen.getByRole('status', { name: 'Progress' })).toHaveTextContent('2,500 of 2,500 deleted');
		await waitFor(() => expect(ondeleted).toHaveBeenCalledTimes(1));
	});

	it('stops after the round in flight and leaves the rest', async () => {
		// Each round resolves only when released, so Stop can land mid-way.
		let release: (() => void) | null = null;
		deleteTraces.mockImplementation(async (filters, confirm, limit) => {
			calls.push({ filters, confirm, limit });
			if (confirm === undefined) {
				return {
					dry_run: true as const,
					matched: 2500,
					would_delete: { traces: 2500, observations: 0, scores: 0, annotation_items: 0 },
					oldest: '2026-09-01T09:00:00Z',
					affected_runs: [],
					confirm: 'acme',
					note: ''
				};
			}
			await new Promise<void>((resolve) => (release = resolve));
			rounds.left -= Math.min(limit ?? 1000, rounds.left);
			return {
				dry_run: false as const,
				deleted: { traces: 1000, observations: 0, scores: 0, payloads: 0, annotation_items: 0 },
				more: rounds.left > 0
			};
		});
		const { user } = dialog({ status: 'error' });
		await user.type(await screen.findByRole('textbox'), 'acme');
		await user.click(screen.getByRole('button', { name: 'Delete these traces' }));

		await user.click(await screen.findByRole('button', { name: 'Stop' }));
		expect(screen.getByRole('button', { name: 'Stopping after this round' })).toBeDisabled();
		release!();

		expect(await screen.findByText(/Stopped after 1,000 traces/)).toBeInTheDocument();
		expect(calls.filter((call) => call.confirm !== undefined)).toHaveLength(1);
		expect(screen.getByRole('status', { name: 'Progress' })).toHaveTextContent('1,000 of 2,500 deleted');
	});
});
