import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Score } from './api/client.svelte';
import { Scores } from './scores.svelte';

// The one read a trace or a session makes (spec 022 #1, #2). What is under
// test here is not the request but what the block is handed *while* one is in
// flight: `TraceDetail` holds one of these across trace ids — the peek panel
// and the session drill-down swap the id without unmounting — and a chip
// carries live Edit and Delete, so a stale row is an offer to retract another
// target's judgement (found in review of PR #41).

const listScores = vi.fn();
const listScoreConfigs = vi.fn(async () => ({ configs: [] as { name: string }[] }));

vi.mock('$lib/api/client.svelte', () => ({
	ApiError: class extends Error {},
	api: {
		listScores: (...args: unknown[]) => listScores(...(args as [])),
		listScoreConfigs: (...args: unknown[]) => listScoreConfigs(...(args as []))
	}
}));

const score = (id: string, trace: string): Score => ({
	id,
	trace_id: trace,
	name: 'accuracy',
	data_type: 'numeric',
	value: 1,
	timestamp: '2026-09-07T10:00:00Z',
	created_at: '2026-09-07T10:00:00Z'
});

/** A read that does not answer until it is told to. */
function held(scores: Score[], cursor: string | null = null) {
	let answer: () => void;
	const gate = new Promise<void>((wake) => (answer = wake));
	return {
		answer: () => answer(),
		read: async () => {
			await gate;
			return { scores, next_cursor: cursor };
		}
	};
}

beforeEach(() => {
	listScores.mockReset();
	listScoreConfigs.mockClear();
});

describe('reading one target’s scores', () => {
	it('drops the previous target’s rows before the new ones arrive', async () => {
		let trace = 'trace-a';
		const scores = new Scores(() => ({ trace_id: trace }));

		listScores.mockResolvedValueOnce({ scores: [score('a1', 'trace-a')], next_cursor: 'more' });
		scores.watch();
		await vi.waitFor(() => expect(scores.rows).toHaveLength(1));
		expect(scores.more).toBe(true);

		// Trace B, with its answer still out.
		const b = held([score('b1', 'trace-b')]);
		listScores.mockImplementationOnce(b.read);
		trace = 'trace-b';
		scores.watch();

		// Nothing of A is on screen while B is in flight: its chips carry
		// Edit and Delete, and they would act on A's rows under B's header.
		expect(scores.rows).toEqual([]);
		expect(scores.more).toBe(false);
		expect(scores.loading).toBe(true);

		b.answer();
		await vi.waitFor(() => expect(scores.rows.map((one) => one.id)).toEqual(['b1']));
	});

	it('keeps the rows on a re-read of the same target', async () => {
		const scores = new Scores(() => ({ trace_id: 'trace-a' }));
		listScores.mockResolvedValue({ scores: [score('a1', 'trace-a')], next_cursor: null });
		scores.watch();
		await vi.waitFor(() => expect(scores.rows).toHaveLength(1));

		// What `refresh()` does after a write of ours: the same target read
		// again, and the block must not blink empty between the two.
		const again = held([score('a1', 'trace-a'), score('a2', 'trace-a')]);
		listScores.mockImplementationOnce(again.read);
		scores.refresh();
		scores.watch();

		expect(scores.rows.map((one) => one.id)).toEqual(['a1']);
		again.answer();
		await vi.waitFor(() => expect(scores.rows).toHaveLength(2));
	});

	it('empties the rows on a failure, and says why', async () => {
		const scores = new Scores(() => ({ trace_id: 'trace-a' }));
		listScores.mockResolvedValueOnce({ scores: [score('a1', 'trace-a')], next_cursor: null });
		scores.watch();
		await vi.waitFor(() => expect(scores.rows).toHaveLength(1));

		listScores.mockRejectedValueOnce(new Error('nope'));
		scores.refresh();
		scores.watch();
		await vi.waitFor(() => expect(scores.failure).not.toBeNull());
		expect(scores.rows).toEqual([]);
	});

	// *Score* is on screen from the first paint and the dialog builds its
	// controls out of the configs, so waiting for the scores before asking for
	// them opens a window where a project that declared names has none (found
	// in review of PR #41).
	it('asks for the configs beside the scores, not after them', async () => {
		const scores = new Scores(() => ({ trace_id: 'trace-a' }));
		const held = new Promise(() => {});
		listScores.mockReturnValueOnce(held);
		listScoreConfigs.mockResolvedValueOnce({ configs: [{ name: 'accuracy' }] });

		scores.watch();

		// The scores read never answers; the configs arrive all the same.
		await vi.waitFor(() => expect(scores.configs).toHaveLength(1));
		expect(scores.loading).toBe(true);
	});

	it('lets an abandoned read answer onto nothing', async () => {
		const scores = new Scores(() => ({ trace_id: 'trace-a' }));
		const a = held([score('a1', 'trace-a')]);
		listScores.mockImplementationOnce(a.read);
		const stop = scores.watch();
		stop();

		a.answer();
		await Promise.resolve();
		expect(scores.rows).toEqual([]);
	});
});
