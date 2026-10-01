import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

/**
 * The mark is drawn twice — the owner's files in `docs/assets/`, and the
 * inline copy the interface wears (spec 006 #30) — and only the files are the
 * owner's. This holds the copy to them: the same box, the same stroke, the
 * same paths in the same colours, and tokens whose values are the files' own.
 */

const read = (path: string) => readFileSync(resolve(process.cwd(), path), 'utf8');

const attr = (svg: string, name: string) => svg.match(new RegExp(`${name}="([^"]+)"`))?.[1];

/** Every path of a master file, as `colour d`. */
function masterPaths(svg: string): string[] {
	return [...svg.matchAll(/<path d="([^"]+)" stroke="(#[0-9A-Fa-f]{6})"\/>/g)].map(
		([, d, stroke]) => `${stroke.toLowerCase()} ${d}`
	);
}

describe('the inline mark', () => {
	const mark = read('src/lib/components/BrandMark.svelte');
	const css = read('src/app.css');
	const light = read('../docs/assets/mark-on-light.svg');
	const dark = read('../docs/assets/mark-on-dark.svg');

	const token = (name: string) =>
		css.match(new RegExp(`--color-${name}:\\s*([^;]+);`))?.[1].toLowerCase();
	const ink = token('brand-ink')?.match(/light-dark\((#[0-9a-f]+),\s*(#[0-9a-f]+)\)/);
	const tree = token('brand-tree');

	it('has the box and the stroke of the files', () => {
		for (const master of [light, dark]) {
			expect(attr(mark, 'viewBox')).toBe(attr(master, 'viewBox'));
			expect(attr(mark, 'stroke-width')).toBe(attr(master, 'stroke-width'));
		}
	});

	it('draws the paths of each file, in its colours, through the tokens', () => {
		expect(ink).toBeTruthy();
		const drawn = (inkColour: string) =>
			[...mark.matchAll(/<g class="stroke-brand-(tree|ink)">([\s\S]*?)<\/g>/g)].flatMap(
				([, group, body]) =>
					[...body.matchAll(/d="([^"]+)"/g)].map(
						([, d]) => `${group === 'tree' ? tree : inkColour} ${d}`
					)
			);
		expect(drawn(ink![1])).toEqual(masterPaths(light));
		expect(drawn(ink![2])).toEqual(masterPaths(dark));
		expect(masterPaths(light)).toHaveLength(5);
	});
});
