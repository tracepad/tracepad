import { render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import PaginationBar from './PaginationBar.svelte';

/**
 * The size control has to be a form field the browser can name: an `id` its
 * label points at, a `name`, and an `id` that stays unique when the bar stands
 * twice on one page — under a listing and inside the session panel over it.
 */

function mount(over: Partial<Parameters<typeof PaginationBar>[1]> = {}) {
	render(PaginationBar, {
		limit: 50,
		rows: 50,
		hasPrev: false,
		hasNext: true,
		atNewest: true,
		atOldest: false,
		onresize: vi.fn(),
		onfirst: vi.fn(),
		onprev: vi.fn(),
		onnext: vi.fn(),
		onlast: vi.fn(),
		noun: 'trace',
		...over
	} as never);
}

describe('the pagination bar', () => {
	it('names the rows-per-page select and points its label at it', () => {
		mount();

		const select = screen.getByLabelText('Rows per page');
		expect(select).toHaveAttribute('name', 'rows-per-page');
		expect(select.id).not.toBe('');
		expect(document.querySelector('label')).toHaveAttribute('for', select.id);
	});

	it('gives each bar on a page its own id', () => {
		mount();
		mount();

		const ids = screen.getAllByLabelText('Rows per page').map((select) => select.id);
		expect(new Set(ids).size).toBe(2);
	});
});
