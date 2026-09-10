import { expect, test, type Page } from '@playwright/test';
import { signIn as enter, state } from './harness';

// Stats over the fixed corpus (Testing). The window is named explicitly rather
// than left to the default: the fixtures carry fixed timestamps, and a suite
// whose assertions depend on how long ago they were generated is a suite that
// starts failing on a quiet Tuesday.

const WINDOW = 'from=2026-08-01T00:00:00Z&to=2026-09-30T00:00:00Z';

async function signIn(page: Page) {
	await enter(page, state().member);
}

test('all four charts render over the corpus', async ({ page }) => {
	await signIn(page);
	await page.goto(`/stats?${WINDOW}&group_by=day`);

	for (const title of ['Traces', 'Cost', 'Latency', 'Errors']) {
		await expect(page.locator('.u-title', { hasText: title })).toBeVisible();
	}
	// One canvas per chart: they are drawn, not merely titled.
	await expect(page.locator('.uplot canvas')).toHaveCount(4);
	// Latency is two series in one chart (spec 007 #6).
	await expect(page.locator('.u-legend', { hasText: 'p95' })).toBeVisible();
	// The header's totals are the endpoint's own numbers.
	await expect(page.getByText('17 traces · 1 with errors')).toBeVisible();
});

test('the breakdown tables match what the endpoint reports', async ({ page }) => {
	await signIn(page);
	await page.goto(`/stats?${WINDOW}&group_by=day`);

	const answer = await fetch(
		`${state().baseURL}/api/v1/stats?${WINDOW}&group_by=model`,
		{ headers: { Authorization: `Bearer ${state().key}` } }
	);
	const { buckets, unit } = (await answer.json()) as {
		unit: string;
		buckets: { key: string; count: number }[];
	};
	expect(buckets.length).toBeGreaterThan(0);

	// The column header names the unit, because a count of observations and a
	// count of traces are not comparable (spec 004 Decision 23).
	expect(unit).toBe('observation');
	const table = page.getByRole('table').filter({ has: page.getByText('Model') });
	for (const bucket of buckets) {
		// The row whose *key* is this model, not every row whose text
		// contains it: `claude-haiku-4-5` is a prefix of
		// `claude-haiku-4-5-20251001`, and the corpus carries both. The key
		// column is the row's header, which is what makes it addressable.
		const row = table
			.getByRole('row')
			.filter({ has: page.getByRole('rowheader', { name: bucket.key, exact: true }) });
		await expect(row).toContainText(String(bucket.count));
	}
});

test('an empty window says so instead of drawing an empty axis', async ({ page }) => {
	await signIn(page);
	await page.goto('/stats?from=2099-01-01T00:00:00Z&to=2099-01-02T00:00:00Z');

	await expect(page.getByText('Nothing in this window').first()).toBeVisible();
	await expect(page.locator('.uplot canvas')).toHaveCount(0);
});

test('the bucket switcher is in the URL and the default follows the window', async ({ page }) => {
	await signIn(page);

	// Under two days of window: hours, without anybody choosing (spec 007 #6).
	await page.goto('/stats?from=2026-08-26T00:00:00Z&to=2026-08-27T00:00:00Z');
	await expect(page.getByRole('button', { name: 'Hourly' })).toHaveAttribute(
		'aria-pressed',
		'true'
	);

	await page.goto(`/stats?${WINDOW}`);
	await expect(page.getByRole('button', { name: 'Daily' })).toHaveAttribute(
		'aria-pressed',
		'true'
	);

	// And a choice is a choice: it survives into the link.
	await page.getByRole('button', { name: 'Hourly' }).click();
	await expect(page).toHaveURL(/group_by=hour/);
});

test('the new screens reach no origin but their own', async ({ page }) => {
	// Fonts are bundled and uPlot ships inside the binary (spec 006 #5).
	const foreign: string[] = [];
	page.on('request', (request) => {
		if (!request.url().startsWith(state().baseURL)) foreign.push(request.url());
	});

	await signIn(page);
	await page.goto(`/stats?${WINDOW}`);
	await page.goto('/sessions');
	await page.goto('/settings');

	expect(foreign).toEqual([]);
});

test('both themes render the charts, and neither scrolls the page sideways', async ({ page }) => {
	await signIn(page);
	await page.goto(`/stats?${WINDOW}&group_by=day`);

	const overflow = async () =>
		page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);

	await page.emulateMedia({ colorScheme: 'light' });
	await expect(page.locator('.uplot canvas').first()).toBeVisible();
	expect(await overflow()).toBeLessThanOrEqual(0);

	await page.emulateMedia({ colorScheme: 'dark' });
	await expect(page.locator('.uplot canvas').first()).toBeVisible();
	expect(await overflow()).toBeLessThanOrEqual(0);
});

// What spec 007 #3's invariant became (spec 028 #4, #6): there is no bearer
// token in the browser at all — the session is a cookie no script can read —
// and what every data-plane request carries instead is the id of the project
// on screen. Checked on the wire, where it matters.
test('a chart is read on the cookie, and names the project it is about', async ({ page }) => {
	await signIn(page);

	const sent: { authorization?: string; project?: string }[] = [];
	page.on('request', (request) => {
		if (request.url().includes('/api/v1/stats') || request.url().includes('/api/v1/sessions')) {
			const headers = request.headers();
			sent.push({
				authorization: headers['authorization'],
				project: headers['x-tracepad-project']
			});
		}
	});

	await page.goto(`/stats?${WINDOW}`);
	await page.goto('/sessions');
	await expect(page.getByText('session-77')).toBeVisible();

	expect(sent.length).toBeGreaterThan(0);
	for (const request of sent) {
		expect(request.authorization).toBeUndefined();
		expect(request.project).toBe(state().project);
	}
});
