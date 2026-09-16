import { render, screen, waitFor } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import DeleteTraceDialog from './DeleteTraceDialog.svelte';

// *Delete…* on the trace header (spec 035 #8): the dialog previews as it
// opens, the echo is the id prefilled, the runs that would lose the trace are
// named, and the host is told once the trace is gone.

const TRACE = '4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f';

const { deleteTrace } = vi.hoisted(() => ({
	deleteTrace: vi.fn(async (_id: string, confirm?: string) =>
		confirm === undefined
			? {
					dry_run: true as const,
					would_delete: { traces: 1, observations: 4, scores: 2, annotation_items: 1 },
					oldest: '2026-09-01T09:00:00Z',
					affected_runs: [{ id: 'a'.repeat(32), dataset: 'golden', traces: 1 }],
					confirm: TRACE,
					note: 'raw OTLP bodies are not deleted; they expire on the raw retention window'
				}
			: {
					dry_run: false as const,
					deleted: { traces: 1, observations: 4, scores: 2, payloads: 3, annotation_items: 1 },
					id: TRACE
				}
	)
}));

vi.mock('$lib/api/client.svelte', () => ({ ApiError: class extends Error {}, api: { deleteTrace } }));

function dialog() {
	const ondeleted = vi.fn();
	const onclose = vi.fn();
	render(DeleteTraceDialog, { props: { open: true, traceID: TRACE, onclose, ondeleted } } as never);
	return { ondeleted, onclose, user: userEvent.setup({ delay: null }) };
}

beforeEach(() => {
	deleteTrace.mockClear();
	document.body.style.pointerEvents = '';
});

describe('the delete-trace dialog', () => {
	it('previews as it opens, names the run, and prefills the echo', async () => {
		dialog();

		// No click: the dialog opened because somebody chose to delete this
		// one thing, so the preview is what it is.
		expect(await screen.findByText('4')).toBeInTheDocument();
		expect(deleteTrace).toHaveBeenCalledWith(TRACE, undefined);
		expect(screen.getByText(/annotation items/)).toBeInTheDocument();
		expect(screen.getByText(/golden/)).toBeInTheDocument();
		expect(screen.getByText(/raw OTLP bodies are not deleted/)).toBeInTheDocument();

		// The id is on screen already; the button is open without typing.
		expect(screen.getByRole('textbox')).toHaveValue(TRACE);
		expect(screen.getByRole('button', { name: 'Delete this trace' })).toBeEnabled();
	});

	it('sends the echo, reports what went, and tells the host', async () => {
		const { ondeleted, user } = dialog();
		await user.click(await screen.findByRole('button', { name: 'Delete this trace' }));

		expect(deleteTrace).toHaveBeenLastCalledWith(TRACE, TRACE);
		expect(await screen.findByRole('status')).toHaveTextContent('Deleted the trace: 4 observations');
		await waitFor(() => expect(ondeleted).toHaveBeenCalledTimes(1));
	});
});
