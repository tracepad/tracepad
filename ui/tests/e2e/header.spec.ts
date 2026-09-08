import { expect, test, type Page } from '@playwright/test';
import { state } from './harness';

// The page header at a phone's width (spec 026 #5), against the real binary.
//
// The two screens are the ones spec 025's DoD parked: a score's detail view and
// a user's page, the two headers that carry a breadcrumb, an identifier and a
// count beside a control. A row that cannot wrap has one way to fit all of that
// into 375 px, which is to squeeze the identifier — the thing the reader came
// for — down to an ellipsis. The contract is that the meta takes a second line
// instead, that the actions stay on the first one, that there is never a third,
// and that the page does not scroll sideways.
//
// Neither identifier has to exist: the header is drawn before the answer
// arrives and is the same header either way, so the case holds on any corpus.
// Both are long enough that the meta cannot fit beside the title at this width,
// which is what makes the wrap the assertion rather than an accident of the
// fixtures.

test.use({ viewport: { width: 375, height: 812 } });

const USER = 'customer-eu-west-1-9f3c2a7b-4d51-11ef-9c2d-0242ac120002';
const SCORE = 'answer-relevance-vs-retrieved-context';

/** One row of the bar: the 48 px floor every screen wears. */
const ROW = 48;

async function signIn(page: Page) {
	await page.goto(state().preAuthed);
	await expect(page).toHaveURL(/\/traces$/);
}

async function header(page: Page) {
	await expect(page.locator('header h1')).toBeVisible();
	return page.evaluate(() => {
		const bar = document.querySelector('header') as HTMLElement;
		const top = (selector: string) => {
			const found = bar.querySelector(selector);
			return found ? found.getBoundingClientRect().top : null;
		};
		return {
			height: bar.getBoundingClientRect().height,
			title: top('h1') ?? 0,
			// The meta is the second half of the wrapping pair; the actions are
			// the header's own last child, outside it.
			meta: top('h1 + div') ?? 0,
			actions: top(':scope > div:last-child') ?? 0,
			scrollWidth: document.documentElement.scrollWidth,
			clientWidth: document.documentElement.clientWidth
		};
	});
}

for (const [screen, path] of [
	['a score', `/quality?name=${SCORE}`],
	['a user', `/users/${USER}`]
] as const) {
	test(`${screen} wears two lines at 375 px, with the actions on the first`, async ({ page }) => {
		await signIn(page);
		await page.goto(path);

		const bar = await header(page);
		// The meta took a line of its own rather than being squeezed into the
		// one the title is on.
		expect(bar.meta).toBeGreaterThan(bar.title + 8);
		// And it wrapped *under* the actions rather than pushing them off the
		// first line, which is the other way a row of three can fold.
		expect(bar.actions).toBeLessThan(bar.meta);
		// Two rows at most: never the three a fixed-height row folded into.
		expect(bar.height).toBeLessThanOrEqual(2 * ROW);
		// And the page itself does not scroll sideways to fit any of it.
		expect(bar.scrollWidth).toBe(bar.clientWidth);
	});
}
