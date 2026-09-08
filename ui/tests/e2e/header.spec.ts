import { expect, test, type Page } from '@playwright/test';
import { state } from './harness';

// The page header at both ends of the rule (spec 026 #5, #14), against the real
// binary.
//
// The two screens are the ones spec 025's DoD parked: a score's detail view and
// a user's page, the two headers that carry a breadcrumb, an identifier and a
// count beside a control. A row that cannot wrap has one way to fit all of that
// into 375 px, which is to squeeze the identifier — the thing the reader came
// for — down to an ellipsis. The contract is that the meta takes a second line
// instead, that the actions stay on the first one, that there is never a third,
// and that the page does not scroll sideways.
//
// And that above a phone nothing moved: the wrap is the narrow screen's answer
// to running out of room, not a new way for a long name to make every desktop
// header two rows tall.
//
// Neither identifier has to exist: the header is drawn before the answer
// arrives and is the same header either way, so the case holds on any corpus.
// Both are long enough that the meta cannot fit beside the title at this width,
// which is what makes the wrap the assertion rather than an accident of the
// fixtures.

const USER = 'customer-eu-west-1-9f3c2a7b-4d51-11ef-9c2d-0242ac120002';
const SCORE = 'answer-relevance-vs-retrieved-context';

// Two hundred characters of score name, which is a name nobody would type and
// every generator writes: the desktop case needs a meta that cannot fit on the
// line so that fitting it is the thing being asserted.
const LONG_SCORE =
	'answer-relevance-vs-retrieved-context-under-the-eu-west-1-rag-pipeline-with-the-reranker-disabled-and-the-summariser-enabled-for-the-support-tier-of-the-quarterly-evaluation-run-of-2026-10-14';

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
		// The identifier itself: the one part of the meta that gives way, and
		// the measurement that says a case about running out of room ran out.
		const named = bar.querySelector('.font-mono');
		return {
			height: bar.getBoundingClientRect().height,
			title: top('h1') ?? 0,
			// The meta is the second half of the wrapping pair; the actions are
			// the header's own last child, outside it.
			meta: top('h1 + div') ?? 0,
			actions: top(':scope > div:last-child') ?? 0,
			clipped: named ? named.scrollWidth > named.clientWidth : false,
			scrollWidth: document.documentElement.scrollWidth,
			clientWidth: document.documentElement.clientWidth
		};
	});
}

test.describe('at a phone', () => {
	test.use({ viewport: { width: 375, height: 812 } });

	for (const [screen, path] of [
		['a score', `/quality?name=${SCORE}`],
		['a user', `/users/${USER}`]
	] as const) {
		test(`${screen} wears two lines at 375 px, with the actions on the first`, async ({
			page
		}) => {
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
});

test.describe('at a desk', () => {
	test.use({ viewport: { width: 1280, height: 800 } });

	test('a name too long for the line is an ellipsis, not a second row', async ({ page }) => {
		await signIn(page);
		await page.goto(`/quality?name=${LONG_SCORE}`);

		const bar = await header(page);
		// The name really is longer than the room it has, which is what makes
		// the rest of this a measurement rather than a coincidence.
		expect(bar.clipped).toBe(true);
		// And the bar it sits in is the one row it has always been, with the
		// meta beside the title rather than under it. The eight pixels are the
		// phone case's threshold read the other way: the two are centred on the
		// same row and differ only by the difference in their type sizes.
		expect(bar.height).toBeLessThan(ROW + 1);
		expect(bar.meta).toBeLessThanOrEqual(bar.title + 8);
		expect(bar.scrollWidth).toBe(bar.clientWidth);
	});
});
