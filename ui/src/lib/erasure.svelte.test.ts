import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Erasure } from './api/client.svelte';
import {
	ERASURE_POLL_MS,
	ErasureWatch,
	confirmErasure,
	describe as say,
	erased,
	settle,
	stage,
	unanswered
} from './erasure.svelte';
import { ApiError } from './api/client.svelte';

// A user-data erasure as the screens say it (spec 044), and as they follow it
// while it runs on the server (spec 047 #18).

const getErasure = vi.fn();
const eraseUserData = vi.fn();

vi.mock('./api/client.svelte', () => ({
	ApiError: class ApiError extends Error {
		constructor(
			public status: number,
			message: string,
			public details: Record<string, unknown> = {}
		) {
			super(message);
		}
	},
	api: {
		erasure: (...args: unknown[]) => getErasure(...args),
		eraseUserData: (...args: unknown[]) => eraseUserData(...args)
	}
}));

function erasure(overrides: Partial<Erasure> = {}): Erasure {
	return {
		id: '4f0c9d3e8a1b2c3d4e5f60718293a4b5',
		state: 'running',
		phase: 'parsed',
		user_id: 'user-4711',
		dry_run: false,
		created_at: '2026-10-02T09:00:00Z',
		started_at: '2026-10-02T09:00:00Z',
		finished_at: null,
		progress: { traces_at_start: 20000, traces_deleted: 5123 },
		deleted: { traces: 5123 },
		compaction: { requested_at: null, expected_by: null, completed_at: null },
		error: null,
		...overrides
	} as Erasure;
}

beforeEach(() => {
	getErasure.mockReset();
	eraseUserData.mockReset();
});
afterEach(() => vi.useRealTimers());

describe('the erasure sentence', () => {
	it('names the traces and what the raw archive lost', () => {
		expect(
			erased('user-4711', {
				traces: 12,
				raw_spans: 252,
				raw_batches_rewritten: 38,
				raw_batches_deleted: 3
			})
		).toBe('Erased 12 traces belonging to user-4711, and 252 spans from 41 raw batches.');
	});

	it('says nothing about the archive when it lost nothing', () => {
		expect(erased('u', { traces: 1, raw_spans: 0 })).toBe('Erased 1 trace belonging to u.');
		expect(erased('u', {})).toBe('Erased 0 traces belonging to u.');
	});
});

describe('where an erasure is', () => {
	it('counts the parsed phase against the traces step 1 found', () => {
		expect(say(erasure(), 'user-4711')).toBe(
			'Erasure in progress — parsed, 5,123 of 20,000 traces'
		);
	});

	it('names the phases it cannot count, and the state before one', () => {
		expect(
			stage(
				erasure({
					phase: 'raw',
					progress: { traces_at_start: 20000, traces_deleted: 0 }
				})
			)
		).toBe('raw');
		expect(stage(erasure({ phase: 'tail' }))).toBe('tail');
		expect(stage(erasure({ state: 'queued', phase: null }))).toBe('queued');
		expect(stage(erasure({ progress: { traces_at_start: null, traces_deleted: 0 } }))).toBe(
			'parsed'
		);
	});

	it('ends in the sentence, or in why it failed and what it took first', () => {
		expect(say(erasure({ state: 'done', phase: null, deleted: { traces: 3 } }), 'u')).toBe(
			'Erased 3 traces belonging to u.'
		);
		expect(
			say(
				erasure({
					state: 'failed',
					phase: null,
					error: 'the disk is full',
					deleted: { traces: 2 }
				}),
				'u'
			)
		).toBe(
			"The erasure of u's data could not finish — the disk is full. Before that, it erased 2 traces belonging to u."
		);
	});
});

describe('a confirmed erasure', () => {
	it('that ended within the wait is its sentence', () => {
		const watch = new ErasureWatch();
		expect(
			settle('p', 'u', erasure({ state: 'done', phase: null, deleted: { traces: 1 } }), watch)
		).toBe('Erased 1 trace belonging to u.');
		expect(watch.running).toBe(false);
	});

	it('that failed within the wait is a failure', () => {
		expect(() =>
			settle('p', 'u', erasure({ state: 'failed', phase: null, error: 'boom' }), new ErasureWatch())
		).toThrow(/could not finish — boom/);
	});

	it('that runs on is followed every two seconds until it ends, and then left alone', async () => {
		vi.useFakeTimers();
		const watch = new ErasureWatch();
		expect(settle('p', 'u', erasure({ state: 'queued', phase: null }), watch)).toMatch(
			/runs on the server/
		);
		expect(watch.running).toBe(true);

		getErasure
			.mockResolvedValueOnce(
				erasure({ progress: { traces_at_start: 20000, traces_deleted: 9000 } })
			)
			.mockResolvedValueOnce(erasure({ state: 'done', phase: null, deleted: { traces: 20000 } }));
		await vi.advanceTimersByTimeAsync(ERASURE_POLL_MS);
		expect(say(watch.current!, 'u')).toBe('Erasure in progress — parsed, 9,000 of 20,000 traces');
		await vi.advanceTimersByTimeAsync(ERASURE_POLL_MS);
		expect(watch.running).toBe(false);
		expect(say(watch.current!, 'u')).toBe('Erased 20000 traces belonging to u.');

		await vi.advanceTimersByTimeAsync(10 * ERASURE_POLL_MS);
		expect(getErasure).toHaveBeenCalledTimes(2);
		expect(getErasure.mock.calls[0].slice(0, 2)).toEqual(['p', '4f0c9d3e8a1b2c3d4e5f60718293a4b5']);
	});

	it('stops being read when the screen stops following it, and keeps what it showed', async () => {
		vi.useFakeTimers();
		const watch = new ErasureWatch();
		watch.follow('p', erasure());
		watch.stop();
		await vi.advanceTimersByTimeAsync(5 * ERASURE_POLL_MS);
		expect(getErasure).not.toHaveBeenCalled();
		expect(watch.current?.state).toBe('running');
	});

	it('stops saying it runs when it is gone', async () => {
		vi.useFakeTimers();
		const watch = new ErasureWatch();
		watch.follow('p', erasure());
		getErasure.mockRejectedValueOnce(new ApiError(404, 'no such erasure'));
		await vi.advanceTimersByTimeAsync(ERASURE_POLL_MS);
		expect(watch.current).toBeNull();
		expect(watch.running).toBe(false);
		await vi.advanceTimersByTimeAsync(5 * ERASURE_POLL_MS);
		expect(getErasure).toHaveBeenCalledTimes(1);
	});
});

describe('a confirmed erasure with no answer', () => {
	const where = 'the list below shows whether it did';

	it('may have been accepted, and says where to look', () => {
		const timedOut = new ApiError(0, 'the server did not answer in time', { timed_out: true });
		expect(unanswered(timedOut, 'u-1', where)).toBe(
			'No answer about erasing the data of u-1: this screen stopped waiting. The server may have ' +
				'accepted the erasure; the list below shows whether it did.'
		);
		for (const status of [502, 504]) {
			expect(unanswered(new ApiError(status, 'Gateway Timeout'), 'u-1', where)).toMatch(
				new RegExp(`a proxy in front of the server stopped waiting \\(${status}\\)`)
			);
		}
	});

	it('leaves every other failure a failure', () => {
		expect(unanswered(new ApiError(0, 'cannot reach the server'), 'u', where)).toBeNull();
		expect(unanswered(new ApiError(400, 'confirm must be the user id'), 'u', where)).toBeNull();
		expect(unanswered(new ApiError(503, 'writes are not available'), 'u', where)).toBeNull();
		expect(unanswered(new Error('boom'), 'u', where)).toBeNull();
	});
});

describe('a screen that moves to another user', () => {
	it('forgets the erasure it followed, and reads it no more', async () => {
		vi.useFakeTimers();
		const watch = new ErasureWatch();
		watch.follow('p', erasure());
		watch.forget();
		expect(watch.current).toBeNull();
		expect(watch.running).toBe(false);
		await vi.advanceTimersByTimeAsync(5 * ERASURE_POLL_MS);
		expect(getErasure).not.toHaveBeenCalled();
	});
});

describe('an answer that comes back late', () => {
	it('is not taken for the erasure the watch follows now', async () => {
		vi.useFakeTimers();
		const watch = new ErasureWatch();
		let answer: (value: Erasure) => void = () => {};
		getErasure.mockImplementationOnce(() => new Promise((resolve) => (answer = resolve)));
		watch.follow('p', erasure({ id: 'a'.repeat(32) }));
		await vi.advanceTimersByTimeAsync(ERASURE_POLL_MS);
		watch.follow('p', erasure({ id: 'b'.repeat(32) }));
		answer(erasure({ id: 'a'.repeat(32), phase: 'tail' }));
		await vi.advanceTimersByTimeAsync(0);
		expect(watch.current?.id).toBe('b'.repeat(32));
		getErasure.mockResolvedValue(erasure({ id: 'b'.repeat(32) }));
		await vi.advanceTimersByTimeAsync(ERASURE_POLL_MS);
		// One read of the one it follows: no second loop.
		expect(getErasure).toHaveBeenCalledTimes(2);
	});

	it('stops being followed once the server refuses to show it', async () => {
		vi.useFakeTimers();
		const watch = new ErasureWatch();
		watch.follow('p', erasure());
		getErasure.mockRejectedValueOnce(new ApiError(403, 'this role may not read erasures'));
		await vi.advanceTimersByTimeAsync(ERASURE_POLL_MS);
		expect(watch.running).toBe(false);
		await vi.advanceTimersByTimeAsync(5 * ERASURE_POLL_MS);
		expect(getErasure).toHaveBeenCalledTimes(1);
	});
});

describe('the confirmed request', () => {
	const ask = (still: boolean, watch: ErasureWatch) => {
		const lost = vi.fn();
		const answered = vi.fn();
		const said = confirmErasure({
			project: 'p',
			user: 'u',
			confirm: 'u',
			watch,
			where: 'look',
			still: () => still,
			lost,
			answered
		});
		return { said, lost, answered };
	};

	it('is followed by the screen it was asked from', async () => {
		vi.useFakeTimers();
		eraseUserData.mockResolvedValueOnce(erasure({ state: 'queued', phase: null }));
		const watch = new ErasureWatch();
		const { said, answered } = ask(true, watch);
		await expect(said).resolves.toMatch(/this dialog follows it/);
		expect(answered).toHaveBeenCalledOnce();
		expect(watch.running).toBe(true);
		expect(eraseUserData).toHaveBeenCalledWith('p', 'u', 'u', 20);
		watch.stop();
	});

	it('is said, and followed by no one, on a screen that moved on', async () => {
		eraseUserData.mockResolvedValueOnce(erasure({ state: 'queued', phase: null }));
		const watch = new ErasureWatch();
		const { said, answered } = ask(false, watch);
		await expect(said).resolves.toBe("The erasure of u's data runs on the server.");
		expect(answered).not.toHaveBeenCalled();
		expect(watch.current).toBeNull();
	});

	it('that got no answer is looked for only by a screen still about it', async () => {
		const timedOut = new ApiError(0, 'the server did not answer in time', { timed_out: true });
		eraseUserData.mockRejectedValue(timedOut);
		const here = ask(true, new ErasureWatch());
		await expect(here.said).rejects.toThrow(/may have accepted the erasure; look\./);
		expect(here.lost).toHaveBeenCalledOnce();
		const gone = ask(false, new ErasureWatch());
		await expect(gone.said).rejects.toThrow(/may have accepted/);
		expect(gone.lost).not.toHaveBeenCalled();
	});
});
