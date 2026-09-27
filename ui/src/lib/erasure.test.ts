import { describe, expect, it } from 'vitest';
import { erased } from './erasure';

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
