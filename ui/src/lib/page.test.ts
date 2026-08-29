import { describe, expect, it } from 'vitest';
import { DEFAULT_PAGE_SIZE, isFirstPage, isLastPage, pageSearch, readPage } from './page';

/**
 * The page is URL state like the filters and the panel beside it (spec 009
 * #1), so the whole of turning, resizing and coming back is a sequence of
 * search strings — testable without a browser and without a route.
 */
describe('the page in the URL', () => {
	const listing = new URLSearchParams('environment=prod&peek=abc');

	it('reads the newest page out of a bare listing', () => {
		expect(readPage(new URLSearchParams(''))).toEqual({
			limit: DEFAULT_PAGE_SIZE,
			cursor: null,
			direction: 'next'
		});
	});

	it('turns a page without disturbing the filters or the panel', () => {
		expect(pageSearch(listing, { cursor: 'CUR' })).toBe('?environment=prod&peek=abc&cursor=CUR');
	});

	it('writes the oldest page as a direction and no cursor', () => {
		expect(pageSearch(new URLSearchParams(''), { direction: 'prev' })).toBe('?direction=prev');
	});

	it('leaves the defaults out of the URL', () => {
		// A URL carries what somebody chose, not the state of the world.
		expect(pageSearch(new URLSearchParams(''), { limit: DEFAULT_PAGE_SIZE })).toBe('');
		expect(pageSearch(new URLSearchParams(''), { direction: 'next' })).toBe('');
	});

	it('drops the cursor when the size changes', () => {
		// A cursor is a position in one paging of one listing; at another
		// size it points into a page that no longer exists.
		const deep = new URLSearchParams('cursor=CUR&direction=prev&limit=100');
		expect(pageSearch(deep, { limit: 25 })).toBe('?limit=25');
	});

	it('takes any size the API takes, and nothing else', () => {
		// The control offers the common steps; the URL may carry any of the
		// 1–500 the endpoint accepts.
		expect(readPage(new URLSearchParams('limit=100')).limit).toBe(100);
		expect(readPage(new URLSearchParams('limit=7')).limit).toBe(7);
		for (const bad of ['0', '501', 'lots', '12.5', '']) {
			expect(readPage(new URLSearchParams(`limit=${bad}`)).limit).toBe(DEFAULT_PAGE_SIZE);
		}
	});

	it('knows the one page live mode means anything on', () => {
		expect(isFirstPage(readPage(new URLSearchParams('')))).toBe(true);
		expect(isFirstPage(readPage(new URLSearchParams('cursor=CUR')))).toBe(false);
		// The oldest page is not the first one, even with no cursor.
		expect(isFirstPage(readPage(new URLSearchParams('direction=prev')))).toBe(false);
	});

	it('knows the two ends, which is a question about the URL', () => {
		// The anchors « and » are answered from here rather than from the
		// cursors of the page that arrived — an empty page has none, and it
		// is exactly the page somebody needs the anchors from.
		const ends = (search: string) => {
			const state = readPage(new URLSearchParams(search));
			return [isFirstPage(state), isLastPage(state)];
		};
		expect(ends('')).toEqual([true, false]);
		expect(ends('direction=prev')).toEqual([false, true]);
		expect(ends('cursor=CUR')).toEqual([false, false]);
		expect(ends('cursor=CUR&direction=prev')).toEqual([false, false]);
	});
});
