import { describe, expect, it } from 'vitest';
import { ApiError } from '$lib/api/client.svelte';
import { Erasure, erased, stillRunning } from './erasure';

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
	it('is what a proxy that stopped waiting answers too', () => {
		for (const status of [502, 504]) {
			expect(stillRunning(new ApiError(status, 'Gateway Timeout'), 'u-1')).toMatch(
				new RegExp(`a proxy in front of the server stopped waiting \\(${status}\\)`)
			);
		}
	});

	it('is still running on the server, not a failure', () => {
		const timedOut = new ApiError(0, 'the server did not answer in time', { timed_out: true });
		expect(stillRunning(timedOut, 'u-1')).toMatch(
			/No answer about erasing the data of u-1: this screen stopped waiting after 30 seconds/
		);
	});

	it('leaves every other failure a failure', () => {
		expect(stillRunning(new ApiError(0, 'cannot reach the server'), 'u')).toBeNull();
		expect(stillRunning(new ApiError(409, 'raw batch 3 was rewritten'), 'u')).toBeNull();
		expect(stillRunning(new ApiError(503, 'writes are not available'), 'u')).toBeNull();
		expect(stillRunning(new Error('boom'), 'u')).toBeNull();
	});
});

describe('a screen erasing a user', () => {
	const timedOut = () => Promise.reject(new ApiError(0, 'no answer in time', { timed_out: true }));
	const answered = () => Promise.resolve({ dry_run: false, deleted: { traces: 1 } });

	it('stays for an erasure left running, and leaves after a retry that answered', async () => {
		const erasure = new Erasure();
		expect(await erasure.ask('u', 'u', timedOut)).toMatch(/runs to the end/);
		expect(erasure.running).toBe(true);
		expect(await erasure.ask('u', 'u', answered)).toEqual({ dry_run: false, deleted: { traces: 1 } });
		expect(erasure.running).toBe(false);
	});

	it('treats a preview that timed out as the failure it is', async () => {
		const erasure = new Erasure();
		await expect(erasure.ask('u', undefined, timedOut)).rejects.toBeInstanceOf(ApiError);
		expect(erasure.running).toBe(false);
	});

	it('passes every other failure on', async () => {
		const erasure = new Erasure();
		const refused = () => Promise.reject(new ApiError(409, 'raw batch 3 was rewritten'));
		await expect(erasure.ask('u', 'u', refused)).rejects.toThrow('raw batch 3');
		expect(erasure.running).toBe(false);
	});
});
