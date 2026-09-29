import { render } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { describe, expect, it } from 'vitest';
import PageHeader from './PageHeader.svelte';

// The header every screen wears (spec 026 #5, #14). `stackBelow` (spec 006 #27)
// is one screen's way of giving its actions a row of their own, and every
// other screen's header is what it was: the classes it carries are pinned here,
// since a layout no pixel test of ours compares is only kept by what is written.

const text = (value: string) => createRawSnippet(() => ({ render: () => `<span>${value}</span>` }));

describe('PageHeader', () => {
	it('is the one row it was without stackBelow', () => {
		const { container } = render(PageHeader, { title: 'Runs', meta: text('m'), actions: text('a') });
		const header = container.querySelector('header')!;

		expect(header.getAttribute('class')).toBe(
			'border-border flex min-h-12 shrink-0 items-center gap-3 border-b px-4'
		);
		const pair = header.firstElementChild!;
		expect(pair.getAttribute('class')).toBe(
			'flex min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-1 sm:flex-nowrap'
		);
		expect(pair.querySelector('div')!.getAttribute('class')).toBe(
			'text-subtle flex min-w-0 items-center gap-2 text-sm'
		);
		expect(header.lastElementChild!.getAttribute('class')).toBe('flex items-center gap-1.5');
	});

	it('gives the actions a row under 1,360 px with stackBelow, and wraps its pair there too', () => {
		const { container } = render(PageHeader, {
			title: 'Run',
			meta: text('m'),
			actions: text('a'),
			stackBelow: 1360
		});
		const header = container.querySelector('header')!;

		expect(header.className).toContain('flex-wrap');
		expect(header.className).toContain('min-[1360px]:flex-nowrap');
		const pair = header.firstElementChild!;
		expect(pair.className).toContain('min-[1360px]:flex-nowrap');
		expect(pair.className).not.toContain('sm:flex-nowrap');
		expect(pair.querySelector('div')!.className).toContain('flex-wrap');
		expect(header.lastElementChild!.className).toContain('w-full');
		expect(header.lastElementChild!.className).toContain('min-[1360px]:w-auto');
	});
});
