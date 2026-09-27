import { describe, expect, it } from 'vitest';
import { ApiError } from '$lib/api/client.svelte';
import { erased, stillRunning } from './erasure';

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

describe('an erasure the screen stopped waiting for', () => {
	it('is still running on the server, not a failure', () => {
		const timedOut = new ApiError(0, 'the server did not answer in time', { timed_out: true });
		expect(stillRunning(timedOut, 'u-1')).toMatch(/still erasing the data of u-1/);
	});

	it('leaves every other failure a failure', () => {
		expect(stillRunning(new ApiError(0, 'cannot reach the server'), 'u')).toBeNull();
		expect(stillRunning(new ApiError(409, 'raw batch 3 was rewritten'), 'u')).toBeNull();
		expect(stillRunning(new Error('boom'), 'u')).toBeNull();
	});
});
