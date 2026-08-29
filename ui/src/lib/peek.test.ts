import { describe, expect, it } from 'vitest';
import type { PageState } from './page';
import { anchor, modified, neighbour, peekSearch, readPeek, selecting, settled, walkable } from './peek';

/**
 * The peek panel is URL state (spec 008 #2), so its whole contract — open,
 * switch rows, drill into a trace, come back, close — is a sequence of search
 * strings, and this is where that sequence is asserted.
 */
describe('the panel in the URL', () => {
	const listing = new URLSearchParams('environment=prod&live=1');

	it('opens a row without disturbing the filters', () => {
		expect(peekSearch(listing, { peek: 'abc' })).toBe('?environment=prod&live=1&peek=abc');
	});

	it('drops the previous row observation when another row opens', () => {
		const open = new URLSearchParams('peek=abc&obs=one');
		expect(peekSearch(open, { peek: 'def' })).toBe('?peek=def');
	});

	it('drills one level, keeping the session it drilled from', () => {
		const open = new URLSearchParams('peek=session-1');
		expect(peekSearch(open, { peek: 'session-1', trace: 'trace-9' })).toBe(
			'?peek=session-1&trace=trace-9'
		);
	});

	it('comes back up without leaving the trace behind', () => {
		const drilled = new URLSearchParams('peek=session-1&trace=trace-9&obs=one');
		expect(peekSearch(drilled, { peek: 'session-1' })).toBe('?peek=session-1');
	});

	it('closes back to the bare listing', () => {
		const drilled = new URLSearchParams('environment=prod&peek=session-1&trace=trace-9&obs=one');
		expect(peekSearch(drilled)).toBe('?environment=prod');
	});

	it('leaves no lone question mark when nothing else is in the URL', () => {
		expect(peekSearch(new URLSearchParams('peek=abc'))).toBe('');
	});

	it('reads back what it wrote', () => {
		expect(readPeek(new URLSearchParams('peek=session-1&trace=trace-9'))).toEqual({
			peek: 'session-1',
			trace: 'trace-9'
		});
		expect(readPeek(new URLSearchParams('environment=prod'))).toEqual({
			peek: null,
			trace: null
		});
	});
});

// One page, newest first as every listing is. The gaps between the minutes
// are where a row the panel is showing can sit without being on the page.
const at = (minute: number) => `2026-08-29T10:${String(minute).padStart(2, '0')}:00Z`;
const page = [
	{ id: 'c', key: at(30) },
	{ id: 'b', key: at(20) },
	{ id: 'a', key: at(10) }
];

describe('walking a page by its order', () => {
	it('moves in both directions', () => {
		expect(neighbour(page, page[1], -1)).toBe('c');
		expect(neighbour(page, page[1], 1)).toBe('a');
	});

	it('stops at either end rather than wrapping', () => {
		expect(neighbour(page, page[0], -1)).toBeNull();
		expect(neighbour(page, page[2], 1)).toBeNull();
		expect(neighbour(page, null, 1)).toBeNull();
	});

	it('finds the nearest row when the panel is off the oldest end', () => {
		// A live tick prepends newer rows and pushes the panel's row off the
		// bottom: everything on screen is newer than it, so `k` steps up into
		// the page and `j` has nothing here and turns it.
		const lost = { id: 'gone', key: at(5) };
		expect(neighbour(page, lost, -1)).toBe('a');
		expect(neighbour(page, lost, 1)).toBeNull();
	});

	it('finds the nearest row when the panel is off the newest end', () => {
		// Turning a page forward with the panel open leaves it behind the
		// other way round, and the answer has to flip with it — one rule for
		// both, which the edge-guessing this replaces could not be.
		const lost = { id: 'gone', key: at(45) };
		expect(neighbour(page, lost, 1)).toBe('c');
		expect(neighbour(page, lost, -1)).toBeNull();
	});

	it('finds both neighbours of a row that fell out of the middle', () => {
		const swept = { id: 'gone', key: at(25) };
		expect(neighbour(page, swept, -1)).toBe('c');
		expect(neighbour(page, swept, 1)).toBe('b');
	});

	it('compares timestamps as instants, not as strings', () => {
		// RFC3339Nano trims trailing zeros, so `:00Z` and `:00.5Z` sort the
		// wrong way round as text — `Z` sorts after `.`.
		const ticks = [
			{ id: 'y', key: '2026-08-29T10:00:00.5Z' },
			{ id: 'x', key: '2026-08-29T10:00:00Z' }
		];
		expect(neighbour(ticks, ticks[1], -1)).toBe('y');
		expect(neighbour(ticks, ticks[0], 1)).toBe('x');
	});

	it('separates two rows inside the same millisecond', () => {
		// What a parallel fan-out produces. The ids run the other way to the
		// instants, so a comparison rounded to milliseconds makes each row
		// invisible to the other and `j` skips one (PR #11, sixth review).
		const burst = [
			{ id: '111', key: '2026-08-29T10:00:00.123456789Z' },
			{ id: '999', key: '2026-08-29T10:00:00.123111111Z' }
		];
		expect(neighbour(burst, burst[0], 1)).toBe('999');
		expect(neighbour(burst, burst[1], -1)).toBe('111');
	});

	it('puts a row the server could not date last, not among the dated ones', () => {
		// The field is absent when the trace carried no start time; placing
		// those by id instead would strand the walk on one of them.
		const undated = [{ id: 'aaa', key: at(10) }, { id: 'bbb', key: at(9) }, { id: 'zzz', key: '' }];
		expect(neighbour(undated, undated[1], 1)).toBe('zzz');
		expect(neighbour(undated, undated[2], -1)).toBe('bbb');
		expect(neighbour(undated, undated[2], 1)).toBeNull();
	});

	it('breaks a tie on the id, the way the server does', () => {
		// Same instant, so the page is ordered by id descending (spec 009 #2).
		const same = [
			{ id: 'b', key: at(10) },
			{ id: 'a', key: at(10) }
		];
		expect(neighbour(same, same[0], 1)).toBe('a');
		expect(neighbour(same, same[1], -1)).toBe('b');
	});
});

describe('a turn that has landed', () => {
	const from = { id: 'gone', key: at(25) };
	const intent = { from, step: 1 as const, cursor: 'X', direction: 'next' as const };
	const spot = (over: Partial<PageState> = {}): PageState =>
		({ limit: 2, cursor: 'X', direction: 'next', ...over });

	it('opens the row nearest the one the walk came from', () => {
		expect(settled(intent, spot(), page)).toBe('b');
	});

	it('leaves a page the walk did not ask for alone', () => {
		expect(settled(intent, spot({ cursor: 'Y' }), page)).toBeNull();
		expect(settled(null, spot(), page)).toBeNull();
	});

	it('tells the two cursors of a one-row page apart by direction', () => {
		// There `prev_cursor === next_cursor`, so a `‹` walk and a `›` click
		// would otherwise settle on each other (PR #11, third review).
		expect(settled(intent, spot({ direction: 'prev' }), page)).toBeNull();
	});

	it('says when a key does nothing, so the control can look dead', () => {
		expect(walkable(page, page[0], 1, null)).toBe(true);
		expect(walkable(page, page[2], 1, null)).toBe(false);
		expect(walkable(page, page[2], 1, 'cursor')).toBe(true);
		// No position to walk from: a page to turn to does not rescue it.
		expect(walkable(page, null, 1, 'cursor')).toBe(false);
	});
});

describe('where the panel says it is', () => {
	const showing = { id: 'gone', key: at(5) };

	it('reads the position off the page when the row is on it', () => {
		// Not off the detail: that lags the URL by a fetch, and a walk held up
		// for it would stutter under a held-down `j`.
		expect(anchor(page, 'a', null)).toEqual(page[2]);
	});

	it('falls back to the detail the panel is showing', () => {
		expect(anchor(page, 'gone', showing)).toBe(showing);
	});

	it('says nothing when neither the page nor the detail knows the row', () => {
		expect(anchor(page, 'gone', null)).toBeNull();
		expect(anchor(page, 'gone', { id: 'other', key: at(5) })).toBeNull();
		expect(anchor(page, null)).toBeNull();
	});
});

describe('which click belongs to the panel', () => {
	const click = (init: Partial<MouseEvent> = {}) =>
		({ button: 0, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, ...init }) as MouseEvent;

	it('takes a plain left click', () => {
		expect(modified(click())).toBe(false);
	});

	it('is not a click at all once text has been selected', () => {
		// The row is clickable end to end, and its values are still values
		// somebody drags a cursor across to copy (spec 008 #15).
		document.body.innerHTML = '<p id="cell">0071122334455667788990aabbccddee</p>';
		const range = document.createRange();
		range.selectNodeContents(document.getElementById('cell') as HTMLElement);
		const selection = window.getSelection();
		selection?.removeAllRanges();
		selection?.addRange(range);

		expect(selecting({ detail: 1 } as MouseEvent)).toBe(true);
		selection?.removeAllRanges();
		expect(selecting({ detail: 1 } as MouseEvent)).toBe(false);
	});

	it('leaves the second click of a double-click to the word it selects', () => {
		expect(selecting({ detail: 2 } as MouseEvent)).toBe(true);
	});

	it('leaves the browser its own gestures', () => {
		// Each of these means "open the page this row points at", and the row
		// still points at it (spec 008 #3).
		expect(modified(click({ metaKey: true }))).toBe(true);
		expect(modified(click({ ctrlKey: true }))).toBe(true);
		expect(modified(click({ shiftKey: true }))).toBe(true);
		expect(modified(click({ altKey: true }))).toBe(true);
		expect(modified(click({ button: 1 }))).toBe(true);
	});
});
