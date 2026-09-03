import { expect, test, type Page } from '@playwright/test';
import { FAILING_TRACE, LARGE_PAYLOAD_OBSERVATION, LARGE_PAYLOAD_TRACE, state } from './harness';

// The payload surface against the real binary (spec 015, Testing): a marker
// loaded from the banner, folding, a search that reaches what folding hid,
// a prompt that wraps rather than scrolling the page sideways, and four
// syntax colours that both themes really do paint differently.
//
// The suite boots with a 4 KiB response budget (`global-setup.ts`), which is
// what puts a truncation marker on the screen to click.

async function signIn(page: Page) {
	await page.goto(state().preAuthed);
	await expect(page).toHaveURL(/\/traces$/);
}

/** The observation whose metadata carries a nested `events` array to fold. */
const NESTED_METADATA_OBSERVATION = '5152535455565758';

/** The trace whose own metadata is a string beside a number. */
const CHAT_TRACE = '4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f';

/**
 * One instance, by the name of its editing region. A trace screen carries
 * several — three payloads and the trace's own metadata — so every locator
 * below is scoped to one of them rather than to the first on the page.
 */
const area = (page: Page, label: string) =>
	page
		.getByLabel(label, { exact: true })
		.locator('xpath=ancestor::div[contains(@class,"cm-editor")][1]');

test('a truncated payload names both sizes, and the banner loads the rest', async ({ page }) => {
	await signIn(page);
	await page.goto(`/traces/${LARGE_PAYLOAD_TRACE}?obs=${LARGE_PAYLOAD_OBSERVATION}`);

	// The preview is a prefix cut on a UTF-8 boundary, so it is shown as text
	// (spec 015 #3) — the old tree could not show it at all.
	const preview = page.getByLabel('Input preview', { exact: true });
	await expect(preview).toContainText('Summarise the release notes');

	// The banner is the choice spec 004 #2 leaves to the consumer: how much is
	// here, how much there is, and the click that spends the budget.
	const banner = page.getByRole('button', { name: /Load the whole/ });
	await expect(banner).toContainText(/showing \d+ B of 3\.8 KB/);

	// The second message's role is past the cut, and so is not on screen.
	await expect(preview).not.toContainText('"user"');

	await banner.click();
	await expect(banner).toHaveCount(0);

	// Swapped for the whole payload, which now reaches past where it was cut.
	// Scrolled to, rather than merely present: a narrow screen wraps this
	// payload past the height CodeMirror draws, which is the point of #5.
	const input = page.getByLabel('Input', { exact: true });
	await input.click();
	await page.keyboard.press('ControlOrMeta+End');
	await expect(input).toContainText('"user"');
});

test('the fold gutter folds a branch and puts it back', async ({ page }) => {
	await signIn(page);
	await page.goto(`/traces/${FAILING_TRACE}?obs=${NESTED_METADATA_OBSERVATION}`);

	const metadata = page.getByLabel('Metadata', { exact: true });
	await expect(metadata).toContainText('TimeoutError');

	// `"events": [` is the branch of this document; folding it takes its
	// contents out of the page entirely.
	// `:visible` because the gutter keeps a hidden marker as its width spacer.
	const gutter = area(page, 'Metadata');
	await gutter.locator('[title="Fold line"]:visible').nth(1).click();
	await expect(metadata).not.toContainText('TimeoutError');

	await gutter.locator('[title="Unfold line"]:visible').first().click();
	await expect(metadata).toContainText('TimeoutError');
});

test('Cmd-F finds a string the page is not rendering', async ({ page }) => {
	await signIn(page);
	await page.goto(`/traces/${FAILING_TRACE}?obs=${NESTED_METADATA_OBSERVATION}`);

	const metadata = page.getByLabel('Metadata', { exact: true });
	await expect(metadata).toContainText('TimeoutError');

	// CodeMirror draws the lines it can see, and a folded branch is not one of
	// them — which is exactly why the browser's own find stops being enough
	// and the panel is part of the surface (#5).
	const area_ = area(page, 'Metadata');
	await area_.locator('[title="Fold line"]:visible').nth(1).click();
	await expect(metadata).not.toContainText('TimeoutError');

	await metadata.click();
	await page.keyboard.press('ControlOrMeta+f');
	// The panel belongs to the document that had focus, and this asserts that
	// it is this one: a search over the payload beside it would find nothing.
	const find = area_.getByLabel('Find', { exact: true });
	await expect(find).toBeVisible();

	// Typed rather than filled: CodeMirror's panel commits its query on keyup,
	// so a value set in one go leaves the first Enter searching for nothing.
	await find.pressSequentially('TimeoutError');
	await find.press('Enter');

	// Found in the document, not in the DOM — and selecting a match inside a
	// folded range opens it, which is how the line comes back.
	await expect(metadata).toContainText('TimeoutError');
	await expect(area_.locator('.cm-searchMatch-selected')).toHaveCount(1);

	// And Escape closes the search panel rather than the page's own (#7).
	await find.press('Escape');
	await expect(find).toHaveCount(0);
	await expect(metadata).toBeVisible();
});

test('Escape inside a payload still closes the peek panel', async ({ page }) => {
	// The other half of #7: with no search panel to close, Escape belongs to
	// the page, and a payload that swallowed it would trap a reader in the
	// panel it was opened from (spec 008).
	await signIn(page);
	await page.goto(`/traces?peek=${FAILING_TRACE}&obs=${NESTED_METADATA_OBSERVATION}`);
	const panel = page.getByRole('dialog');
	await expect(panel).toBeVisible();

	await page.getByLabel('Metadata', { exact: true }).click();
	await page.keyboard.press('Escape');

	await expect(panel).toHaveCount(0);
});

test('a long prompt wraps, and nothing scrolls sideways', async ({ page }) => {
	test.skip(test.info().project.name !== 'mobile', 'the narrow width is the test');
	await signIn(page);
	await page.goto(`/traces/${LARGE_PAYLOAD_TRACE}?obs=${LARGE_PAYLOAD_OBSERVATION}`);

	await page.getByRole('button', { name: /Load the whole/ }).click();
	const input = page.getByLabel('Input', { exact: true });
	await expect(input).toContainText('The exporter batches spans');

	// A 3.7 KB string on a 375 px screen: wrapped, so the document scrolls
	// down inside its own box and the page does not scroll across (design §8).
	const inside = await area(page, 'Input')
		.locator('.cm-scroller')
		.evaluate((node) => node.scrollWidth - node.clientWidth);
	expect(inside).toBeLessThanOrEqual(1);

	const across = await page.evaluate(
		() => document.documentElement.scrollWidth - window.innerWidth
	);
	expect(across).toBeLessThanOrEqual(0);
});

test('both themes paint the four code colours, and paint them differently', async ({ page }) => {
	await signIn(page);
	// The trace's own metadata — `{"channel": "web", "retries": 0}` — is a key,
	// a string, a number and punctuation in four lines.
	await page.goto(`/traces/${CHAT_TRACE}`);
	await expect(page.getByLabel('Trace metadata', { exact: true })).toContainText('channel');

	const colours = () =>
		page.evaluate(() => {
			const region = document.querySelector('[aria-label="Trace metadata"]');
			const seen = new Set<string>();
			for (const span of region?.querySelectorAll('span') ?? []) {
				seen.add(getComputedStyle(span).color);
			}
			return [...seen].sort();
		});

	await page.evaluate(() => (document.documentElement.dataset.theme = 'light'));
	const light = await colours();
	// Keys, strings, numbers and punctuation, each its own token (#6).
	expect(light.length).toBeGreaterThanOrEqual(4);

	await page.evaluate(() => (document.documentElement.dataset.theme = 'dark'));
	const dark = await colours();
	expect(dark.length).toBe(light.length);
	// The theme reaches inside the editor: no shipped palette of its own.
	expect(dark).not.toEqual(light);
});
