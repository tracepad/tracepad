import { expect, test, type Page } from '@playwright/test';
import { createProject, signIn as enter, state } from './harness';

// The Quality screens (spec 025, Testing — e2e), against the real binary.
//
// In a project of its own (spec 016 #18): the overview draws a card per score
// name in the window, so the assertions are about *which* names are there, and
// the default corpus's own scores would add to them. The traffic is exported as
// OTLP protobuf, the way an SDK exports it, and the scores are posted as JSON,
// the way an eval loop posts them.

/** Two hours in the past, so the hours are closed from the first pass. */
const HOUR_A = 1787738400000000000n; // 2026-08-26T10:00:00Z
const HOUR_B = 1787745600000000000n; // 2026-08-26T12:00:00Z
/** A window wide enough to hold both, for the URLs that ask for them. */
const WINDOW = 'from=2026-08-01T00:00:00Z&to=2026-09-30T00:00:00Z';

/**
 * A third hour, deliberately *outside* `WINDOW`: it holds the fifty-one names
 * the ceiling of spec 025 #24 truncates, and the tests above count the cards of
 * their own window.
 */
const HOUR_C = 1784109600000000000n; // 2026-07-15T10:00:00Z
const CROWDED_WINDOW = 'from=2026-07-01T00:00:00Z&to=2026-07-31T00:00:00Z';

const GRADED = 'a0'.padEnd(32, '1');
const GRADED_SPAN = 'a0'.padEnd(16, '1');
const SECOND = 'b0'.padEnd(32, '2');
const SECOND_SPAN = 'b0'.padEnd(16, '2');
const CROWDED = 'c0'.padEnd(32, '3');
const CROWDED_SPAN = 'c0'.padEnd(16, '3');

let own: ReturnType<typeof createProject> | null = null;
const project = () => (own ??= createProject('quality'));

/**
 * One breakdown panel by its heading. Not `getByRole('table')`: an empty
 * breakdown draws a sentence instead of a table, and a locator that can only
 * find the populated one cannot assert the empty case at all.
 */
const panel = (page: Page, title: string) =>
	page.locator('section').filter({ has: page.getByRole('heading', { name: title }) });

async function signIn(page: Page) {
	await enter(page, (await project()).account);
}

// --- a minimal OTLP/protobuf export ------------------------------------------
//
// The same builder `users.spec.ts` uses. A generation carries a model, which is
// what makes the model breakdown of an observation-level score non-empty.

function varint(n: number): number[] {
	const out: number[] = [];
	while (n > 127) {
		out.push((n & 127) | 128);
		n = Math.floor(n / 128);
	}
	out.push(n);
	return out;
}
const field = (num: number, wire: number, payload: number[]) => [
	...varint((num << 3) | wire),
	...payload
];
const bytes = (num: number, payload: number[]) => field(num, 2, [...varint(payload.length), ...payload]);
const text = (num: number, value: string) => bytes(num, [...Buffer.from(value, 'utf8')]);
const fixed64 = (num: number, value: bigint) => {
	const buffer = Buffer.alloc(8);
	buffer.writeBigUInt64LE(value);
	return field(num, 1, [...buffer]);
};
const hex = (value: string) => [...Buffer.from(value, 'hex')];
const attribute = (key: string, value: string) => [...text(1, key), ...bytes(2, text(1, value))];

type Span = { trace: string; span: string; at: bigint; environment: string; release: string };

function exportOf(spans: Span[]): Uint8Array {
	const encoded = spans.map((span) =>
		bytes(2, [
			...bytes(1, hex(span.trace)),
			...bytes(2, hex(span.span)),
			...text(5, 'answer'),
			...fixed64(7, span.at),
			...fixed64(8, span.at + 700_000_000n),
			...bytes(9, attribute('gen_ai.request.model', 'claude-sonnet-5')),
			...bytes(9, attribute('deployment.environment.name', span.environment)),
			// `langfuse.release` on the span rather than `service.version` on
			// the resource: two traces of one export need two releases, and a
			// span's `service.version` names a peer rather than this run
			// (spec 012's mapping).
			...bytes(9, attribute('langfuse.release', span.release))
		])
	);
	const resource = bytes(1, bytes(1, attribute('service.name', 'quality-e2e')));
	const scope = bytes(1, text(1, 'e2e'));
	return Uint8Array.from(bytes(1, [...resource, ...bytes(2, [...scope, ...encoded.flat()])]));
}

async function deliver(spans: Span[]) {
	const { baseURL } = state();
	const { key } = await project();
	const response = await fetch(`${baseURL}/v1/traces`, {
		method: 'POST',
		headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/x-protobuf' },
		body: Buffer.from(exportOf(spans))
	});
	if (!response.ok) throw new Error(`ingest: ${response.status} ${await response.text()}`);
}

async function score(scores: Record<string, unknown>[]) {
	const { baseURL } = state();
	const { key } = await project();
	const response = await fetch(`${baseURL}/api/v1/scores`, {
		method: 'POST',
		headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
		body: JSON.stringify(scores)
	});
	if (!response.ok) throw new Error(`scores: ${response.status} ${await response.text()}`);
}

/**
 * Waits for the aggregator to take the delivered hours, by asking the endpoint
 * what the rollup holds for a range that is entirely behind the watermark.
 * Polling rather than sleeping keeps the wait honest: it *is* the lag the docs
 * publish.
 */
async function rolled(name: string) {
	const { baseURL } = state();
	const { key } = await project();
	await expect
		.poll(
			async () => {
				const response = await fetch(
					`${baseURL}/api/v1/stats/scores?name=${name}&group_by=day&${WINDOW.replace(/&/g, '&')}`,
					{ headers: { Authorization: `Bearer ${key}` } }
				);
				if (!response.ok) return -1;
				const body = (await response.json()) as { series: { buckets: unknown[] }[] };
				return body.series[0]?.buckets.length ?? 0;
			},
			{ timeout: 30_000, message: 'the aggregator never rolled the graded hours' }
		)
		.toBeGreaterThan(0);
}

let seeded: Promise<void> | null = null;

/** The corpus this file reads: idempotent, so both Playwright projects may run it. */
function seed(): Promise<void> {
	seeded ??= (async () => {
		await deliver([
			{ trace: GRADED, span: GRADED_SPAN, at: HOUR_A + 10_000_000_000n, environment: 'production', release: '2.5.0' },
			{ trace: SECOND, span: SECOND_SPAN, at: HOUR_B + 10_000_000_000n, environment: 'staging', release: '2.6.0' },
			{ trace: CROWDED, span: CROWDED_SPAN, at: HOUR_C + 10_000_000_000n, environment: 'production', release: '2.4.0' }
		]);
		// Fifty-one names on one trace, an hour outside `WINDOW`: one more than
		// the endpoint returns, which is what the overview has to admit to.
		await score(
			Array.from({ length: 51 }, (_, i) => ({
				id: `c${i.toString(16).padStart(2, '0')}`.padEnd(32, '9'),
				trace_id: CROWDED,
				name: `metric-${i.toString().padStart(2, '0')}`,
				value: 0.5
			}))
		);
		await score([
			// Numeric on the trace, twice, so a mean is a mean of something.
			{ id: 'aa'.padEnd(32, '1'), trace_id: GRADED, name: 'hallucination', value: 0.2 },
			{ id: 'aa'.padEnd(32, '2'), trace_id: SECOND, name: 'hallucination', value: 0.6 },
			// Boolean and categorical, so all three card shapes are drawn.
			{ id: 'aa'.padEnd(32, '3'), trace_id: GRADED, name: 'thumbs', data_type: 'boolean', value: 1 },
			{ id: 'aa'.padEnd(32, '4'), trace_id: SECOND, name: 'thumbs', data_type: 'boolean', value: 0 },
			{ id: 'aa'.padEnd(32, '5'), trace_id: GRADED, name: 'verdict', data_type: 'categorical', string_value: 'pass' },
			// On the observation, which is the only kind the model breakdown
			// may count (spec 025 #6).
			{
				id: 'aa'.padEnd(32, '6'),
				trace_id: GRADED,
				observation_id: GRADED_SPAN,
				name: 'faithfulness',
				value: 0.9
			},
			// Neither of these belongs on a timeline, and the screen must not
			// draw a card for them (spec 025 #1).
			{ id: 'aa'.padEnd(32, '7'), session_id: 'sess-quality', name: 'csat', value: 1 },
			{ id: 'aa'.padEnd(32, '8'), trace_id: GRADED, name: 'why', data_type: 'text', string_value: 'it made it up' }
		]);
		await rolled('hallucination');
	})();
	return seeded;
}

test.beforeEach(async () => {
	await seed();
});

test('the overview shows a card per score name, with points', async ({ page }) => {
	await signIn(page);
	await page.getByRole('link', { name: 'Quality' }).click();
	await expect(page).toHaveURL(/\/quality$/);

	await page.goto(`/quality?${WINDOW}&group_by=day`);
	// One card per series, each an actual chart rather than a titled frame.
	for (const name of ['hallucination', 'thumbs', 'verdict', 'faithfulness']) {
		await expect(page.getByRole('link', { name: new RegExp(name) })).toBeVisible();
	}
	await expect(page.locator('.uplot canvas')).toHaveCount(4);
	// A session-only score and a text score have no trace hour to sit in.
	await expect(page.getByRole('link', { name: /csat/ })).toHaveCount(0);
	await expect(page.getByRole('link', { name: /\bwhy\b/ })).toHaveCount(0);
});

test('a card opens the detail view with the trend and the three breakdowns', async ({ page }) => {
	await signIn(page);
	await page.goto(`/quality?${WINDOW}&group_by=day`);
	await page.getByRole('link', { name: /hallucination/ }).click();

	await expect(page).toHaveURL(/name=hallucination/);
	// The window survived the click, which is what makes the card a link.
	await expect(page).toHaveURL(/from=2026-08-01/);
	// The trend and the count chart beside it.
	await expect(page.locator('.uplot canvas')).toHaveCount(2);
	await expect(page.locator('.u-legend', { hasText: 'Mean' })).toBeVisible();

	// The three breakdowns, over the three groupings.
	await expect(panel(page, 'By environment').getByRole('row').filter({ hasText: 'production' })).toBeVisible();
	await expect(panel(page, 'By environment').getByRole('row').filter({ hasText: 'staging' })).toBeVisible();
	await expect(panel(page, 'By release').getByRole('row').filter({ hasText: '2.5.0' })).toBeVisible();
	// `hallucination` grades traces, not observations, so no model can carry
	// it — and an empty breakdown says so rather than drawing an empty table.
	await expect(panel(page, 'By model').getByText('Nothing in this window')).toBeVisible();

	// The back link returns to the overview and keeps the window.
	await page.getByRole('link', { name: 'All scores' }).click();
	await expect(page).toHaveURL(/\/quality\?from=2026-08-01/);
});

test('the model breakdown lists the observation score model, and only that', async ({ page }) => {
	await signIn(page);
	await page.goto(`/quality?${WINDOW}&group_by=day&name=faithfulness`);

	const models = panel(page, 'By model');
	await expect(models.getByRole('row').filter({ hasText: 'claude-sonnet-5' })).toBeVisible();
	await expect(models.getByRole('row')).toHaveCount(2); // the header and the one model
});

test('the environment box and the range narrow the numbers', async ({ page }) => {
	await signIn(page);
	await page.goto(`/quality?${WINDOW}&group_by=day&name=hallucination`);

	// Both scores are in the window, so the environments breakdown has two
	// rows under its header.
	await expect(page.getByRole('link', { name: 'All scores' })).toBeVisible();
	const environments = panel(page, 'By environment');
	await expect(environments.getByRole('row')).toHaveCount(3);

	await page.getByPlaceholder('Environment').fill('production');
	await page.getByPlaceholder('Environment').blur();
	await expect(page).toHaveURL(/environment=production/);
	await expect(environments.getByRole('row')).toHaveCount(2);
	await expect(environments.getByRole('row').filter({ hasText: 'staging' })).toHaveCount(0);

	// A window that holds neither is the detail view's own empty state, not a
	// 404 (spec 025 #12).
	await page.goto('/quality?from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z&name=hallucination');
	await expect(page.getByText('No score named')).toBeVisible();
});

// The request bodies, not the row count: a screen that asked for the wrong
// window would still render rows (the brief's own lesson from spec 024).
test('the detail view asks for the series and the three groupings, once each', async ({ page }) => {
	await signIn(page);

	const asked: string[] = [];
	page.on('request', (request) => {
		const url = new URL(request.url());
		if (url.pathname === '/api/v1/stats/scores') asked.push(url.search);
	});

	await page.goto(`/quality?${WINDOW}&group_by=day&name=hallucination`);
	await expect(page.locator('.uplot canvas').first()).toBeVisible();
	await expect.poll(() => asked.length, { timeout: 5000 }).toBeGreaterThanOrEqual(4);
	await page.waitForTimeout(300);

	const groupings = asked.map((search) => new URLSearchParams(search).get('group_by')).sort();
	expect(groupings).toEqual(['day', 'environment', 'model', 'release']);
	for (const search of asked) {
		const params = new URLSearchParams(search);
		expect(params.get('name')).toBe('hallucination');
		expect(params.get('from')).toBe('2026-08-01T00:00:00Z');
		expect(params.get('to')).toBe('2026-09-30T00:00:00Z');
	}
});

test('a score posted after the pass appears after the next one', async ({ page }) => {
	const late = 'late-' + Math.random().toString(36).slice(2, 6);
	await score([
		{ id: 'cc'.padEnd(32, '9'), trace_id: SECOND, name: late, value: 0.5 }
	]);

	await signIn(page);
	// The trace's hour is behind the watermark, so this name is in the answer
	// only once the pass that its arrival dirtied has run (spec 025 #3).
	await expect
		.poll(
			async () => {
				await page.goto(`/quality?${WINDOW}&group_by=day`);
				return page.getByRole('link', { name: new RegExp(late) }).count();
			},
			{ timeout: 30_000, message: 'the late score never reached the rollup' }
		)
		.toBe(1);
});

// A grid of fifty that looked complete would be the one thing worse than a
// truncated one (spec 025 #24).
test('the overview admits what the ceiling left out', async ({ page }) => {
	await signIn(page);
	await page.goto(`/quality?${CROWDED_WINDOW}&group_by=day`);

	await expect(page.getByText('50 of 51 score names')).toBeVisible();
	await expect(page.getByText('1 rarer score name not shown')).toBeVisible();
	// The busiest fifty are what came back, and every one of them is a card.
	await expect(page.locator('.uplot canvas')).toHaveCount(50);
});

test('an empty window says how a score is recorded', async ({ page }) => {
	await signIn(page);
	await page.goto('/quality?from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z');

	await expect(page.getByText('No score names a trace in this window')).toBeVisible();
	await expect(page.getByText('tracepad.score(')).toBeVisible();
	await expect(page.getByRole('link', { name: 'the quality guide' })).toBeVisible();
});

test('375 px never scrolls the page sideways', async ({ page }) => {
	await signIn(page);
	const overflow = async () =>
		page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);

	await page.setViewportSize({ width: 375, height: 812 });
	await page.goto(`/quality?${WINDOW}&group_by=day`);
	await expect(page.locator('.uplot canvas').first()).toBeVisible();
	expect(await overflow()).toBeLessThanOrEqual(0);

	await page.goto(`/quality?${WINDOW}&group_by=day&name=hallucination`);
	await expect(page.locator('.uplot canvas').first()).toBeVisible();
	expect(await overflow()).toBeLessThanOrEqual(0);
});
