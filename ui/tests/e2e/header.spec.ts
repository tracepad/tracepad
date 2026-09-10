import { expect, test, type Page } from '@playwright/test';
import { createProject, signIn as enter, state, type Account } from './harness';

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
	await enter(page, state().member);
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

// --- the trace whose header is the measurement -------------------------------
//
// A trace of this file's own, in a project of its own (spec 016 #18): the two
// long identifiers are the pressure the header has to survive, and the shared
// corpus carries short ones on purpose.

const TRACE = 'aa11bb22cc33dd44ee55ff6600778899';
const SESSION = 'checkout-eu-west-1-2026-09-09-7c1d4e88-4d51-11ef-9c2d-0242ac120002';

const attribute = (key: string, value: string) => ({ key, value: { stringValue: value } });

let sown: Promise<Account> | null = null;

/**
 * One span in the OTLP/JSON encoding, which is what an SDK on
 * `OTEL_EXPORTER_OTLP_PROTOCOL=http/json` writes (spec 019 #7) — and a body
 * this file can spell out, where the protobuf the other suites build takes a
 * varint encoder to say the same thing. Returns the project's own account.
 */
function sow(): Promise<Account> {
	sown ??= (async () => {
		const own = await createProject('header');
		const { baseURL } = state();
		const at = (BigInt(Date.now()) - 60_000n) * 1_000_000n;
		const response = await fetch(`${baseURL}/v1/traces`, {
			method: 'POST',
			headers: { Authorization: `Bearer ${own.key}`, 'Content-Type': 'application/json' },
			body: JSON.stringify({
				resourceSpans: [
					{
						resource: { attributes: [attribute('service.name', 'header-e2e')] },
						scopeSpans: [
							{
								scope: { name: 'e2e' },
								spans: [
									{
										traceId: TRACE,
										spanId: 'a1b2c3d4e5f60718',
										name: 'answer-question',
										startTimeUnixNano: String(at),
										endTimeUnixNano: String(at + 1_234_000_000n),
										attributes: [
											attribute('user.id', USER),
											attribute('session.id', SESSION)
										]
									}
								]
							}
						]
					}
				]
			})
		});
		if (!response.ok) throw new Error(`ingest: ${response.status} ${await response.text()}`);
		// Delivery is not readability: the export returns when the batch is
		// accepted and the writer commits it after.
		await expect
			.poll(
				async () =>
					(
						await fetch(`${baseURL}/api/v1/traces/${TRACE}`, {
							headers: { Authorization: `Bearer ${own.key}` }
						})
					).status,
				{ timeout: 15_000, message: 'the sown trace never became readable' }
			)
			.toBe(200);
		return own.account;
	})();
	return sown;
}

// A trace's own header carries the rule spec 023 #17c decided not to fold into
// `TracePeekMeta`: the short parts do not shrink, and the ids are what gives
// way. `TracePeekMeta` has that pinned by a unit test over its classes, and
// this header — the one that actually shipped the defect, in PR #47 — had
// nothing. Ten hundred pixels wide is where it bites: wide enough for the two
// ids to be shown at all (`md`), narrow enough that they do not fit.
test.describe('a trace at 900 px', () => {
	test.use({ viewport: { width: 900, height: 800 } });

	test('the timestamp stays on one line and the bar stays one row tall', async ({ page }) => {
		await enter(page, await sow());
		await page.goto(`/traces/${TRACE}`);
		await expect(page.locator('header h1')).toHaveText('answer-question');

		const bar = await page.evaluate(() => {
			const header = document.querySelector('header') as HTMLElement;
			const meta = header.querySelector('h1 + div') as HTMLElement;
			// The timestamp is the meta's first span, after the breadcrumb
			// link: the release and the two ids that follow are what may be
			// cut, and this is not one of them.
			const stamp = meta.querySelector('span.font-mono') as HTMLElement;
			const clipped = (element: Element | null) =>
				element ? element.scrollWidth > element.clientWidth : false;
			return {
				height: header.getBoundingClientRect().height,
				// Height against line height, because a timestamp allowed to
				// shrink does not clip: it has spaces in it, so it wraps, and
				// the row it is in grows to hold both lines.
				stamp: {
					text: stamp.textContent?.trim() ?? '',
					height: stamp.getBoundingClientRect().height,
					lineHeight: parseFloat(getComputedStyle(stamp).lineHeight)
				},
				// The ids really did run out of room, which is what makes the
				// rest of this a measurement rather than a coincidence.
				squeezed: [...meta.querySelectorAll('a.font-mono')].filter(clipped).length,
				scrollWidth: document.documentElement.scrollWidth,
				clientWidth: document.documentElement.clientWidth
			};
		});

		expect(bar.squeezed).toBe(2);
		expect(bar.stamp.text).not.toBe('');
		expect(bar.stamp.height).toBeLessThanOrEqual(bar.stamp.lineHeight);
		expect(bar.height).toBeLessThanOrEqual(ROW);
		expect(bar.scrollWidth).toBe(bar.clientWidth);
	});
});

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
