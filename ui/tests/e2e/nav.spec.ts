import { expect, test, type Page } from '@playwright/test';
import { section, signIn as enter, state } from './harness';

// The shell's navigation at both widths (spec 006 #20): on a desktop the
// column of every section; on a phone a 48px bar on top, four tabs under the
// thumb and *More* for the rest in a sheet that Escape, the backdrop and a
// swipe down all close.

async function signIn(page: Page) {
	await enter(page, state().member);
}

test('a desktop keeps the column and has no More', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'desktop', 'the column is the wide shape');
	await signIn(page);

	const nav = page.getByRole('navigation', { name: 'Sections', exact: true });
	await expect(nav.getByRole('link')).toHaveCount(11);
	await expect(page.getByRole('button', { name: 'More' })).toHaveCount(0);
});

test('a phone has four tabs under the page and the rest in More', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile', 'the tab bar is the narrow shape');
	await signIn(page);

	const nav = page.getByRole('navigation', { name: 'Sections', exact: true });
	await expect(nav.getByRole('link')).toHaveText(['Dashboard', 'Traces', 'Sessions', 'Users']);
	// The bar is under the listing, at the bottom of the viewport, and every
	// tab is a thumb's size.
	const viewport = page.viewportSize()!;
	const bar = (await nav.boundingBox())!;
	expect(bar.y + bar.height).toBeCloseTo(viewport.height, 0);
	for (const tab of await nav.getByRole('link').all()) {
		expect((await tab.boundingBox())!.height).toBeGreaterThanOrEqual(44);
	}
	const main = (await page.locator('main').boundingBox())!;
	expect(main.y).toBeLessThanOrEqual(48);
	expect(main.y + main.height).toBeLessThanOrEqual(bar.y + 1);
	// And in the document as on the screen, so Tab reaches the page first.
	const after = await page.evaluate(() => {
		const main = document.querySelector('main')!;
		const tabs = document.querySelector('nav[aria-label="Sections"]')!;
		return !!(main.compareDocumentPosition(tabs) & Node.DOCUMENT_POSITION_FOLLOWING);
	});
	expect(after).toBe(true);

	// A screen *More* holds: the tab is lit and keeps its name, and the sheet
	// lights the screen.
	// Open, the sheet keeps the focus: Tab goes round it and not out of it.
	const queues = await section(page, 'Queues');
	for (let i = 0; i < 12; i++) await page.keyboard.press('Tab');
	const inside = await page.evaluate(
		() => !!document.activeElement?.closest('[role="dialog"]')
	);
	expect(inside).toBe(true);
	await queues.click();
	await expect(page).toHaveURL(/\/queues$/);
	await expect(page.getByRole('dialog')).toHaveCount(0);
	const more = nav.getByRole('button', { name: 'More' });
	await expect(more).toHaveAttribute('aria-current', 'true');
	await expect(more).toHaveText('More');
	await expect(await section(page, 'Queues')).toHaveAttribute('aria-current', 'page');

	await page.keyboard.press('Escape');
	await expect(page.getByRole('dialog')).toHaveCount(0);
	await expect(more).toBeFocused();
});

test('a swipe down the grip closes the sheet, a short one does not', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile', 'the sheet is the narrow shape');
	await signIn(page);
	await page.getByRole('button', { name: 'More' }).click();
	const sheet = page.getByRole('dialog', { name: 'More' });
	await expect(sheet).toBeVisible();

	const grip = sheet.locator('[data-grip]');
	const drag = async (distance: number) => {
		const box = (await grip.boundingBox())!;
		const x = box.x + box.width / 2;
		const y = box.y + box.height / 2;
		const touch = { pointerType: 'touch', pointerId: 7, isPrimary: true, bubbles: true };
		await grip.dispatchEvent('pointerdown', { ...touch, clientX: x, clientY: y });
		await grip.dispatchEvent('pointermove', { ...touch, clientX: x, clientY: y + distance });
		await grip.dispatchEvent('pointerup', { ...touch, clientX: x, clientY: y + distance });
	};

	await drag(30);
	await expect(sheet).toBeVisible();
	await drag(120);
	await expect(sheet).toHaveCount(0);
});
