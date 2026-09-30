import { describe, expect, it } from 'vitest';
import { drawn, type Line } from './chart';

const line = (label: string, values: (number | null)[], hidden = false): Line => ({
	label,
	values,
	token: 'accent',
	hidden
});

describe('drawn', () => {
	const input = line('Input', [1, 2]);
	const reasoning = line('Reasoning', [3, null], true);

	it('draws the default: hidden entries stay out of the plot', () => {
		const lines = [input, reasoning];
		expect(lines.map((one) => drawn(lines, new Map(), one))).toEqual([true, false]);
	});

	it('keeps the reader\'s choice across a rebuild', () => {
		const lines = [input, reasoning];
		const chosen = new Map([['Reasoning', true], ['Input', false]]);
		expect(lines.map((one) => drawn(lines, chosen, one))).toEqual([false, true]);
	});

	it('draws a hidden entry when it is the only one with data', () => {
		const lines = [line('Input', [null, null]), reasoning];
		expect(lines.map((one) => drawn(lines, new Map(), one))).toEqual([true, true]);
	});
});
