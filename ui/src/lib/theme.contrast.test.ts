import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

/**
 * The accessibility floor of spec 006, checked rather than claimed: every
 * text colour in the token file reaches 4.5:1 against every surface it can
 * land on, in **both** themes. Contrast is the one thing a second theme
 * silently breaks, and it breaks in the theme nobody on the team uses.
 *
 * The palette is read out of `app.css`, so a token added or nudged there is
 * covered without anybody remembering to come back here.
 */

type Palette = Record<string, { light: string; dark: string }>;

/** Every `--color-x: light-dark(a, b)` declaration in the `@theme` block. */
function palette(): Palette {
	const css = readFileSync(resolve(process.cwd(), 'src/app.css'), 'utf8');
	const pairs: Palette = {};
	const declaration = /--color-([a-z0-9-]+):\s*light-dark\(\s*(#[0-9a-f]{3,8})\s*,\s*(#[0-9a-f]{3,8})\s*\)/gi;
	for (const [, name, light, dark] of css.matchAll(declaration)) {
		pairs[name] = { light, dark };
	}
	return pairs;
}

function luminance(hex: string): number {
	const channels = [1, 3, 5].map((at) => parseInt(hex.slice(at, at + 2), 16) / 255);
	const [r, g, b] = channels.map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4));
	return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

function contrast(a: string, b: string): number {
	const [high, low] = [luminance(a), luminance(b)].sort((x, y) => y - x);
	return (high + 0.05) / (low + 0.05);
}

/** Colours that carry words, and must therefore be readable as text. */
const TEXT = [
	'fg',
	'muted',
	'subtle',
	'accent',
	'danger',
	'warn',
	'ok',
	'code-string',
	'code-number'
];

/** Colours a screen paints behind those words. */
const SURFACES = ['canvas', 'surface', 'raised'];

describe('the token palette', () => {
	const colours = palette();

	it('was actually read', () => {
		// A regex that stopped matching would make every assertion below pass
		// vacuously, which is the failure mode of a check like this.
		expect(Object.keys(colours).length).toBeGreaterThanOrEqual(TEXT.length + SURFACES.length);
	});

	for (const theme of ['light', 'dark'] as const) {
		for (const text of TEXT) {
			for (const surface of SURFACES) {
				it(`reads ${text} on ${surface} in ${theme}`, () => {
					const ratio = contrast(colours[text][theme], colours[surface][theme]);
					expect(ratio).toBeGreaterThanOrEqual(4.5);
				});
			}
		}

		it(`reads on-accent against accent in ${theme}`, () => {
			const ratio = contrast(colours['on-accent'][theme], colours.accent[theme]);
			expect(ratio).toBeGreaterThanOrEqual(4.5);
		});

		// Tinted backgrounds carry the same words as the plain ones, and so
		// does the one solid danger surface: spec 007's destructive buttons
		// put `on-accent` on `danger`, which is a pairing no other screen had.
		for (const [text, tint] of [
			['fg', 'accent-soft'],
			['accent', 'accent-soft'],
			['danger', 'danger-soft'],
			['on-accent', 'danger']
		] as const) {
			it(`reads ${text} on ${tint} in ${theme}`, () => {
				expect(contrast(colours[text][theme], colours[tint][theme])).toBeGreaterThanOrEqual(4.5);
			});
		}
	}
});
