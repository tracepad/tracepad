import { describe, expect, it } from 'vitest';
import { fold, highlight, searchTerms } from './search';

// Highlighting a search hit (spec 011, Testing): the interface folds the query
// terms the way the tokenizer does, so what it marks is what actually matched.

describe('folding', () => {
	it('folds case and diacritics like the tokenizer', () => {
		expect(fold('Refund')).toBe('refund');
		expect(fold('RÉFUND')).toBe('refund');
		// Precomposed and decomposed spellings of the same word are one word.
		expect(fold('réfund')).toBe('refund');
		expect(fold('réfund')).toBe('refund');
		expect(fold('Ärger')).toBe('arger');
	});
});

describe('the terms a query asks for', () => {
	it('reads bare words, phrases and a trailing star', () => {
		expect(searchTerms('Refund order')).toEqual([
			{ words: ['refund'], prefix: false },
			{ words: ['order'], prefix: false }
		]);
		expect(searchTerms('"refund order"')).toEqual([{ words: ['refund', 'order'], prefix: false }]);
		expect(searchTerms('err*')).toEqual([{ words: ['err'], prefix: true }]);
	});

	it('splits identifiers on punctuation, as the tokenizer does', () => {
		expect(searchTerms('user_id_42')).toEqual([{ words: ['user', 'id', '42'], prefix: false }]);
	});

	it('asks for nothing when there is no word in it', () => {
		expect(searchTerms('')).toEqual([]);
		expect(searchTerms('()-:^')).toEqual([]);
	});
});

describe('marking the hit', () => {
	const marked = (text: string, query: string) =>
		highlight(text, searchTerms(query))
			.map((piece) => (piece.hit ? `[${piece.text}]` : piece.text))
			.join('');

	it('marks the words the search asked for, whatever their case', () => {
		expect(marked('The Refund failed', 'refund')).toBe('The [Refund] failed');
	});

	it('marks a word the query spelled with an accent', () => {
		expect(marked('a réfund was issued', 'refund')).toBe('a [réfund] was issued');
	});

	it('marks whole words only', () => {
		expect(marked('errors and terror', 'error')).toBe('errors and terror');
		expect(marked('errors and terror', 'error*')).toBe('[errors] and terror');
	});

	it('marks every word of a phrase', () => {
		expect(marked('the refund order stands', '"refund order"')).toBe(
			'the [refund] [order] stands'
		);
	});

	it('leaves text alone when nothing was asked for', () => {
		expect(highlight('anything at all', [])).toEqual([{ text: 'anything at all', hit: false }]);
		expect(highlight('', searchTerms('refund'))).toEqual([]);
	});
});
