import { describe, expect, it, vi } from 'vitest';
import { ITEMS, MORE, SECTIONS, TABS } from './sections';
import { SCREENS } from './screens';

vi.mock('$app/state', () => ({ page: { url: new URL('http://tracepad.test/') } }));

describe('the navigation', () => {
	it('lists each screen once, by a one-segment path', () => {
		const hrefs = SCREENS.map((screen) => screen.href);
		expect(new Set(hrefs).size).toBe(hrefs.length);
		for (const href of hrefs) expect(href).toMatch(/^\/[a-z-]+$/);
	});

	it('draws every screen of the list, with an icon, the Evals ones under their label', () => {
		expect(ITEMS.map((item) => item.href)).toEqual(SCREENS.map((screen) => screen.href));
		for (const item of ITEMS) expect(item.icon, item.href).toBeTruthy();
		const evals = SECTIONS.find((section) => 'children' in section);
		expect(evals && 'children' in evals && evals.children.map((child) => child.href)).toEqual(
			SCREENS.filter((screen) => 'group' in screen).map((screen) => screen.href)
		);
	});

	it('gives every screen to the tabs or to More', () => {
		const more = MORE.flatMap((one) => ('children' in one ? one.children : [one]));
		expect([...TABS, ...more].map((item) => item.href).sort()).toEqual(
			ITEMS.map((item) => item.href).sort()
		);
	});
});
