import { render, screen } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import type { Trace } from '$lib/api/client.svelte';
import TracePeekMeta from './TracePeekMeta.svelte';

// The parts a panel says about its trace, and the breakpoints they appear at
// (spec 026 #4). The classes are asserted rather than the layout: what stood in
// six files was five `hidden …:inline` spans, and the defect this component
// exists to prevent is one of them being changed in one file and not in five.
// The session, which spec 023 #17 added, is the sixth and the only link.

const trace = {
	id: 'ff6677008899001122aabb33cc44dd55',
	timestamp: '2026-09-06T14:32:10Z',
	release: '2026.9.1',
	session_id: 'session-77',
	latency_ms: 1234,
	total_cost: 0.0215
} as unknown as Trace;

const parts = (container: HTMLElement) =>
	[...container.querySelectorAll('span')].map((span) => span.className);

describe('the trace peek meta', () => {
	it('renders the five parts, each at the width it earns', () => {
		const { container } = render(TracePeekMeta, { trace } as never);

		expect(parts(container)).toEqual([
			'hidden shrink-0 font-mono sm:inline',
			'hidden truncate md:inline',
			'hidden shrink-0 tabular-nums md:inline',
			'hidden shrink-0 tabular-nums md:inline',
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
			'hidden shrink-0 font-mono sm:inline',
			'hidden shrink-0 tabular-nums md:inline',
			'hidden shrink-0 tabular-nums md:inline'
		]);
		expect(screen.queryByTitle('Release')).not.toBeInTheDocument();
		expect(screen.queryByText(trace.id)).not.toBeInTheDocument();
		// The button stays: a site can copy an id it does not show.
		expect(screen.getByRole('button', { name: 'Copy the trace id' })).toBeInTheDocument();
	});

	// The panel is the path spec 008 #3 calls the normal one, so the session is
	// a destination here as well as in the table and on the full page.
	it('makes the session a link, between the release and the latency', () => {
		const { container } = render(TracePeekMeta, { trace } as never);

		const link = screen.getByRole('link', { name: 'session-77' });
		expect(link).toHaveAttribute('href', '/sessions/session-77');
		expect(link).toHaveAttribute('title', 'Everything in session-77');
		expect(link.className).toBe('hover:text-fg hidden truncate font-mono hover:underline md:inline');
		// Where it sits, because a meta read out of order is a meta nobody reads.
		expect([...container.children].map((part) => part.tagName)).toEqual([
			'SPAN', // the timestamp
			'SPAN', // the release
			'A', // the session
			'SPAN', // the latency
			'SPAN', // the cost
			'SPAN', // the id
			'BUTTON'
		]);
	});

	it('leaves the session out where the reader is already in it', () => {
		render(TracePeekMeta, { trace, hide: ['session'] } as never);

		expect(screen.queryByRole('link')).not.toBeInTheDocument();
		expect(screen.queryByText('session-77')).not.toBeInTheDocument();
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
