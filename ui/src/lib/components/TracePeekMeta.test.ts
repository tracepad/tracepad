import { render, screen } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import type { Trace } from '$lib/api/client.svelte';
import TracePeekMeta from './TracePeekMeta.svelte';

// The five parts a panel says about its trace, and the breakpoints they appear
// at (spec 026 #4). The classes are asserted rather than the layout: what stood
// in six files was five `hidden …:inline` spans, and the defect this component
// exists to prevent is one of them being changed in one file and not in five.

const trace = {
	id: 'ff6677008899001122aabb33cc44dd55',
	timestamp: '2026-09-06T14:32:10Z',
	release: '2026.9.1',
	latency_ms: 1234,
	total_cost: 0.0215
} as unknown as Trace;

const parts = (container: HTMLElement) =>
	[...container.querySelectorAll('span')].map((span) => span.className);

describe('the trace peek meta', () => {
	it('renders the five parts, each at the width it earns', () => {
		const { container } = render(TracePeekMeta, { trace } as never);

		expect(parts(container)).toEqual([
			'hidden font-mono sm:inline',
			'hidden truncate md:inline',
			'hidden tabular-nums md:inline',
			'hidden tabular-nums md:inline',
			'hidden truncate font-mono lg:inline'
		]);
		expect(screen.getByTitle('Release')).toHaveTextContent('2026.9.1');
		expect(screen.getByText(trace.id)).toBeInTheDocument();
		expect(screen.getByRole('button', { name: 'Copy the trace id' })).toBeInTheDocument();
	});

	it('leaves out the parts a site names, and only those', () => {
		const { container } = render(TracePeekMeta, {
			trace,
			hide: ['release', 'id']
		} as never);

		expect(parts(container)).toEqual([
			'hidden font-mono sm:inline',
			'hidden tabular-nums md:inline',
			'hidden tabular-nums md:inline'
		]);
		expect(screen.queryByTitle('Release')).not.toBeInTheDocument();
		expect(screen.queryByText(trace.id)).not.toBeInTheDocument();
		// The button stays: a site can copy an id it does not show.
		expect(screen.getByRole('button', { name: 'Copy the trace id' })).toBeInTheDocument();
	});

	it('renders nothing at all without a trace or an id', () => {
		const { container } = render(TracePeekMeta, { trace: null } as never);

		expect(container.textContent?.trim()).toBe('');
		expect(container.querySelector('button')).toBeNull();
	});

	it('carries an id the trace has not brought yet', () => {
		// A queue's item names the trace it points at before the trace lands.
		const { container } = render(TracePeekMeta, { id: 'abc' } as never);

		expect(parts(container)).toEqual(['hidden truncate font-mono lg:inline']);
		expect(screen.getByText('abc')).toBeInTheDocument();
		expect(screen.getByRole('button', { name: 'Copy the trace id' })).toBeInTheDocument();
	});
});
