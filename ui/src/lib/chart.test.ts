import { describe, expect, it } from 'vitest';
import { drawn, lonely, type Line } from './chart';

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

describe('lonely', () => {
	it('finds a value between two gaps', () => {
		expect(lonely([null, 4, null, null])).toEqual([1]);
	});

	it('counts the ends of the series as gaps', () => {
		expect(lonely([3, null, null, 5])).toEqual([0, 3]);
		expect(lonely([7])).toEqual([0]);
	});

	it('leaves a value with a neighbour to the line', () => {
		expect(lonely([null, 1, 2, null, 3])).toEqual([4]);
		expect(lonely([1, 2, 3])).toEqual([]);
	});

	it('treats zero as a value, never as a gap', () => {
		expect(lonely([0, null, 0, 0])).toEqual([0]);
	});

	it('finds nothing in an empty or all-gap series', () => {
		expect(lonely([])).toEqual([]);
		expect(lonely([null, null])).toEqual([]);
	});
});
