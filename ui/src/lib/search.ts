// Highlighting a search hit (spec 011, Application contract). The API answers
// with data, not markup — the snippet comes back as plain text — so a client
// that wants to mark the terms folds them the way the tokenizer does.
//
// Pure, and a mirror of `ParseSearch` in the store: bare words, `"quoted
// phrases"` and a trailing `*`, everything else punctuation the tokenizer drops.

/** One thing the search asked for, folded. */
export type SearchTerm = { words: string[]; prefix: boolean };

/** A run of the snippet, and whether it is part of a hit. */
export type Piece = { text: string; hit: boolean };

/** Letters, digits and the marks that belong to them — `unicode61`'s alphabet. */
const WORD = /[\p{L}\p{N}\p{M}]+/gu;

/**
 * Folds case and diacritics, as `remove_diacritics 2` does inside the index.
 * NFD splits an accented letter into its base and a combining mark, which is
 * exactly what has to go.
 */
export function fold(text: string): string {
	return text.normalize('NFD').replace(/\p{M}/gu, '').toLowerCase();
}

/** The terms a query asked for. Text with no word in it asks for nothing. */
export function searchTerms(query: string): SearchTerm[] {
	const terms: SearchTerm[] = [];
	// Quoted runs first, so the words inside one stay together; what is left
	// between them is bare words.
	for (const token of query.match(/"[^"]*"?|\S+/g) ?? []) {
		const quoted = token.startsWith('"');
		const body = quoted ? token.slice(1).replace(/"$/, '') : token.replace(/\*+$/, '');
		const words = (fold(body).match(WORD) ?? []).filter(Boolean);
		if (words.length) terms.push({ words, prefix: !quoted && token.endsWith('*') });
	}
	return terms;
}

/**
 * Splits text into the runs to mark and the runs to leave alone.
 *
 * Word by word rather than phrase by phrase: a phrase's words are adjacent in
 * anything the server sent back as a match, so marking each of them marks the
 * phrase, and a reader is looking for their words rather than auditing the
 * server's adjacency rule.
 */
export function highlight(text: string, terms: SearchTerm[]): Piece[] {
	if (!terms.length || !text) return text ? [{ text, hit: false }] : [];
	const pieces: Piece[] = [];
	let at = 0;
	for (const match of text.matchAll(WORD)) {
		const start = match.index;
		if (!hits(fold(match[0]), terms)) continue;
		if (start > at) pieces.push({ text: text.slice(at, start), hit: false });
		pieces.push({ text: match[0], hit: true });
		at = start + match[0].length;
	}
	if (at < text.length) pieces.push({ text: text.slice(at), hit: false });
	return pieces;
}

function hits(word: string, terms: SearchTerm[]): boolean {
	return terms.some((term) =>
		term.words.some(
			(wanted, i) =>
				word === wanted ||
				(term.prefix && i === term.words.length - 1 && word.startsWith(wanted))
		)
	);
}
