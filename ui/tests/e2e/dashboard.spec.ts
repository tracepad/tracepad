import { expect, test, type Page } from '@playwright/test';
import { signIn as enter, signInAsOwner, state } from './harness';

// The dashboard over the fixed corpus (spec 007 Testing, spec 034). The
// window is named explicitly rather than left to the default: the fixtures
// carry fixed timestamps, and a suite whose assertions depend on how long ago
// they were generated is a suite that starts failing on a quiet Tuesday.

const WINDOW = 'from=2026-08-01T00:00:00Z&to=2026-09-30T00:00:00Z';

async function signIn(page: Page) {
	await enter(page, state().member);
}

test('all five charts render over the corpus', async ({ page }) => {
	await signIn(page);
	await page.goto(`/dashboard?${WINDOW}&group_by=day`);

	for (const title of ['Traces', 'Cost', 'Tokens', 'Latency', 'Errors']) {
		await expect(page.locator('.u-title', { hasText: title })).toBeVisible();
	}
	// One canvas per chart: they are drawn, not merely titled.
	await expect(page.locator('.uplot canvas')).toHaveCount(5);
	// Latency is two series in one chart (spec 007 #6).
	await expect(page.locator('.u-legend', { hasText: 'p95' })).toBeVisible();
	// The summary row's figures are the endpoint's own numbers (spec 034 #2).
	const summary = page.getByLabel('Summary');
	await expect(summary.getByText('Traces')).toBeVisible();
	await expect(summary.getByText('21', { exact: true })).toBeVisible();
	// And a window whose previous window predates the corpus reads *new*.
	await expect(summary.getByText('new').first()).toBeVisible();
	// The last-trace line reads the listing with no window (spec 034 #4).
	await expect(page.getByText(/^Last trace /)).toBeVisible();
});

test('the Tokens chart has three lines and the model table a column, from the corpus', async ({
	page
}) => {
	await signIn(page);
	await page.goto(`/dashboard?${WINDOW}&group_by=day`);

	// Three classes in one chart (spec 031 #6); the corpus carries all three
	// spellings and a cache-read count, so every line has a point.
	const legend = page.locator('.uplot', { has: page.locator('.u-title', { hasText: 'Tokens' }) })
		.locator('.u-legend');
	for (const label of ['Input', 'Output', 'Cache read']) {
		await expect(legend).toContainText(label);
	}

	// The model table's column: the row of the model with the most
	// observations shows its own input plus output.
	const byModel = await fetch(
		`${state().baseURL}/api/v1/stats?${WINDOW}&group_by=model`,
		{ headers: { Authorization: `Bearer ${state().key}` } }
	);
	const models = (await byModel.json()) as {
		buckets: { key: string; count: number; tokens?: { input?: number; output?: number } }[];
	};
	const model = models.buckets.find((bucket) => bucket.tokens?.input !== undefined)!;
	const table = page.getByRole('table').filter({ has: page.getByText('Model') });
	const row = table
		.getByRole('row')
		.filter({ has: page.getByRole('rowheader', { name: model.key, exact: true }) });
	await expect(table.getByRole('columnheader', { name: 'Tokens' })).toBeVisible();
	await expect(row).toContainText(
		((model.tokens!.input ?? 0) + (model.tokens!.output ?? 0)).toLocaleString('en-US')
	);
});

test('the breakdown tables match what the endpoint reports', async ({ page }) => {
	await signIn(page);
	await page.goto(`/dashboard?${WINDOW}&group_by=day`);

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
	await page.goto('/dashboard?from=2099-01-01T00:00:00Z&to=2099-01-02T00:00:00Z');

	await expect(page.getByText('Nothing in this window').first()).toBeVisible();
	await expect(page.locator('.uplot canvas')).toHaveCount(0);
});

test('the bucket switcher is in the URL and the default follows the window', async ({ page }) => {
	await signIn(page);

	// Under two days of window: hours, without anybody choosing (spec 007 #6).
	await page.goto('/dashboard?from=2026-08-26T00:00:00Z&to=2026-08-27T00:00:00Z');
	await expect(page.getByRole('button', { name: 'Hourly' })).toHaveAttribute(
		'aria-pressed',
		'true'
	);

	await page.goto(`/dashboard?${WINDOW}`);
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
	await page.goto(`/dashboard?${WINDOW}`);
	await page.goto('/sessions');
	await page.goto('/settings');

	expect(foreign).toEqual([]);
});

test('both themes render the charts, and neither scrolls the page sideways', async ({ page }) => {
	await signIn(page);
	await page.goto(`/dashboard?${WINDOW}&group_by=day`);

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

	await page.goto(`/dashboard?${WINDOW}`);
	await page.goto('/sessions');
	await expect(page.getByText('session-77')).toBeVisible();

	expect(sent.length).toBeGreaterThan(0);
	for (const request of sent) {
		expect(request.authorization).toBeUndefined();
		expect(request.project).toBe(state().project);
	}
});

// The one screen that is the front page (spec 034 #1): `/`, `/p/{id}` and the
// old `/stats` address all land on it, with the query kept.
test('the dashboard is where a project opens, and /stats redirects to it', async ({ page }) => {
	await signIn(page);
	const id = /\/p\/([0-9a-f]{32})\//.exec(page.url())![1];

	await page.goto(`/p/${id}`);
	await expect(page).toHaveURL(new RegExp(`/p/${id}/dashboard`));
	await page.goto(`/stats?${WINDOW}&group_by=hour`);
	await expect(page).toHaveURL(new RegExp(`/p/${id}/dashboard\\?${WINDOW}&group_by=hour$`));
	await expect(page.getByRole('heading', { name: 'Dashboard' })).toBeVisible();
	// First in the sidebar, and there is no Stats row any more.
	const nav = page.getByRole('navigation', { name: 'Sections' });
	await expect(nav.getByRole('link').first()).toHaveText('Dashboard');
	await expect(nav.getByRole('link', { name: 'Stats' })).toHaveCount(0);
});

// Customize (spec 034 #8, #9): a block hidden here is hidden after a reload,
// because the arrangement is the account's and lives on the server; Reset
// puts it back.
test('a hidden block stays hidden across a reload, and Reset restores it', async ({ page }) => {
	await signIn(page);
	await page.goto(`/dashboard?${WINDOW}&group_by=day`);
	await expect(page.locator('.u-title', { hasText: 'Tokens' })).toBeVisible();

	await page.getByRole('button', { name: 'Customize' }).click();
	const written = page.waitForResponse(
		(response) => response.url().endsWith('/api/v1/auth/me') && response.request().method() === 'PATCH'
	);
	await page.getByRole('button', { name: 'Hide Tokens' }).click();
	await written;
	await expect(page.locator('.u-title', { hasText: 'Tokens' })).toHaveCount(0);
	await expect(page.getByLabel('Hidden blocks')).toContainText('Tokens');
	await page.getByRole('button', { name: 'Done' }).click();

	await page.reload();
	await expect(page.locator('.u-title', { hasText: 'Traces' })).toBeVisible();
	await expect(page.locator('.u-title', { hasText: 'Tokens' })).toHaveCount(0);
	// And it is not asked for: the timeline serves the other four charts.

	await page.getByRole('button', { name: 'Customize' }).click();
	await page.getByRole('button', { name: 'Reset' }).click();
	await expect(page.locator('.u-title', { hasText: 'Tokens' })).toBeVisible();
	await page.getByRole('button', { name: 'Done' }).click();
});

// The pointer path of Customize (spec 034 #8): a block dragged by its handle
// lands where it was dropped, and the order is the account's afterwards.
test('a block dragged by its handle lands above the summary', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name === 'mobile', 'a drag is a pointer gesture');
	// As the owner: the arrangement is the account's, and the member's is
	// being written by the test above, which may run at the same time.
	await signInAsOwner(page, state().project);
	await page.goto(`/dashboard?${WINDOW}&group_by=day`);
	// The charts settle the layout; a box measured before they draw is stale.
	await expect(page.locator('.uplot canvas')).toHaveCount(5);
	await page.getByRole('button', { name: 'Customize' }).click();

	const handle = page.getByRole('button', { name: 'Move Errors' });
	await handle.hover();
	const from = (await handle.boundingBox())!;
	const to = (await page.getByRole('listitem', { name: 'Summary' }).boundingBox())!;
	await page.mouse.move(from.x + from.width / 2, from.y + from.height / 2);
	await page.mouse.down();
	await page.mouse.move(from.x + from.width / 2 + 8, from.y + from.height / 2 + 8, { steps: 3 });
	await page.mouse.move(to.x + 60, to.y + 30, { steps: 20 });
	await page.waitForTimeout(300);
	// The drop is written once the drop animation has ended; a reload
	// before the answer would abort the write.
	const written = page.waitForResponse(
		(response) => response.url().endsWith('/api/v1/auth/me') && response.request().method() === 'PATCH'
	);
	await page.mouse.up();
	await written;

	const blocks = page.locator('[aria-label="Dashboard blocks"] > [aria-label]');
	await expect(blocks.first()).toHaveAttribute('aria-label', 'Errors');
	await page.getByRole('button', { name: 'Done' }).click();
	await page.reload();
	await expect(page.locator('.u-title').first()).toHaveText('Errors');

	// Back to the default for the tests that follow.
	await page.getByRole('button', { name: 'Customize' }).click();
	await page.getByRole('button', { name: 'Reset' }).click();
	await expect(blocks.first()).toHaveAttribute('aria-label', 'Summary');
});

// The remembered window (spec 034 #7): a preset set on the dashboard is the
// window Quality opens on, and it is not the window Traces opens on.
test('the window is remembered for the screens that open on one', async ({ page }, testInfo) => {
	// The presets open above the bar at a phone width and the top row lands
	// past the viewport's edge (on every screen, before this spec); what is
	// under test is the memory, which the desktop run covers.
	test.skip(testInfo.project.name === 'mobile', 'the preset popover is off screen on a phone');
	await signIn(page);
	await page.goto(`/dashboard?${WINDOW}`);
	await page.getByRole('button', { name: /^Time range: / }).click();
	await page.getByRole('button', { name: 'Last 24 hours' }).click();
	await expect(page).toHaveURL(/from=/);
	await expect(page).not.toHaveURL(/to=/);

	await page.goto('/quality');
	await expect(page.getByRole('button', { name: /^Time range: Last 24 hours/ })).toBeVisible();
	await page.goto('/traces');
	await expect(page.getByRole('button', { name: /^Time range: Any time/ })).toBeVisible();
});
