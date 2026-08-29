import { describe, expect, it } from 'vitest';
import { modified, neighbour, peekSearch, readPeek, selecting } from './peek';

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

describe('walking the rows that are loaded', () => {
	const ids = ['a', 'b', 'c'];

	it('moves in both directions', () => {
		expect(neighbour(ids, 'b', -1)).toBe('a');
		expect(neighbour(ids, 'b', 1)).toBe('c');
	});

	it('stops at either end rather than wrapping', () => {
		expect(neighbour(ids, 'a', -1)).toBeNull();
		expect(neighbour(ids, 'c', 1)).toBeNull();
	});

	it('has no neighbour for a row that is not on screen', () => {
		// A live poll can drop the row the panel is showing out of the page.
		expect(neighbour(ids, 'gone', 1)).toBeNull();
		expect(neighbour(ids, null, 1)).toBeNull();
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
