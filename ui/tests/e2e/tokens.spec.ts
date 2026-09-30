import { expect, test, type Locator, type Page } from '@playwright/test';
import { createProject, signIn as enter, state } from './harness';

// Tokens on the screens (spec 049 PR 2, Testing — E2E), against the real
// binary: the Tokens column on the three tables and its tooltip, the Min
// tokens filter, the Users sort, the user page's breakdown column and the
// dashboard's legend. In a project of its own, because the Users listing
// answers from the rollup and its order is the assertion (spec 016 #18).

const HEAVY = 'heavy@e2e';
const LIGHT = 'light@e2e';
const NONE = 'none@e2e';
const HOUR = 3_600_000_000_000n;
// On the hour and in the past, so the hours are closed from the aggregator's
// first pass, and relative to now so the default window still holds them.
const HOUR_A = (BigInt(Date.now()) * 1_000_000n / HOUR - 6n) * HOUR;
const iso = (nanos: bigint) => new Date(Number(nanos / 1_000_000n)).toISOString();
const WINDOW = `from=${iso(HOUR_A - 24n * HOUR)}&to=${iso(HOUR_A + 24n * HOUR)}`;

/**
 * The tooltip a pointer over `text` gets: hovered, and read off the nearest
 * element with a title. A wrapper's own attribute says nothing when a
 * descendant carries a title of its own.
 */
async function tooltipOver(text: Locator): Promise<string | null> {
	await text.hover();
	return text.evaluate((node) => node.closest('[title]')?.getAttribute('title') ?? null);
}

/** The cell under the Tokens header, by position, in a row of the visible table. */
async function tokensCell(page: Page, row: Locator): Promise<Locator> {
	const heads = await page.getByRole('columnheader').allTextContents();
	return row.getByRole('cell').nth(heads.map((one) => one.trim()).indexOf('Tokens'));
}

let own: ReturnType<typeof createProject> | null = null;
const project = () => (own ??= createProject('tokens'));

async function signIn(page: Page) {
	await enter(page, (await project()).account);
}

// --- a minimal OTLP/protobuf export (see users.spec.ts for the builder) ------

function varint(n: number): number[] {
	const out: number[] = [];
	while (n > 127) {
		out.push((n & 127) | 128);
		n = Math.floor(n / 128);
	}
	out.push(n);
	return out;
}
const field = (num: number, wire: number, payload: number[]) => [...varint((num << 3) | wire), ...payload];
const bytes = (num: number, payload: number[]) => field(num, 2, [...varint(payload.length), ...payload]);
const text = (num: number, value: string) => bytes(num, [...Buffer.from(value, 'utf8')]);
const fixed64 = (num: number, value: bigint) => {
	const buffer = Buffer.alloc(8);
	buffer.writeBigUInt64LE(value);
	return field(num, 1, [...buffer]);
};
const hex = (value: string) => [...Buffer.from(value, 'hex')];
const attribute = (key: string, value: string) => [...text(1, key), ...bytes(2, text(1, value))];
/** KeyValue{key, value: AnyValue{int_value}} — field 3 of AnyValue. */
const integer = (key: string, value: number) => [...text(1, key), ...bytes(2, field(3, 0, varint(value)))];

type Usage = Record<string, number>;
type Span = { trace: string; name: string; user: string; session: string; at: bigint; usage: Usage };

function exportOf(spans: Span[]): Uint8Array {
	const encoded = spans.map((span) =>
		bytes(2, [
			...bytes(1, hex(span.trace)),
			...bytes(2, hex(span.trace.slice(0, 16))),
			...text(5, span.name),
			...fixed64(7, span.at),
			...fixed64(8, span.at + 700_000_000n),
			...bytes(9, attribute('user.id', span.user)),
			...bytes(9, attribute('session.id', span.session)),
			...bytes(9, attribute('gen_ai.request.model', 'tokens-model')),
			...Object.entries(span.usage).flatMap(([key, value]) =>
				bytes(9, integer(`gen_ai.usage.${key}`, value))
			)
		])
	);
	const resource = bytes(1, bytes(1, attribute('service.name', 'tokens-e2e')));
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

/** Waits for the aggregator to roll the delivered hours (the listing is the rollup alone). */
async function rolled(expected: number) {
	const { baseURL } = state();
	const { key } = await project();
	await expect
		.poll(
			async () => {
				const response = await fetch(`${baseURL}/api/v1/users`, {
					headers: { Authorization: `Bearer ${key}` }
				});
				if (!response.ok) return -1;
				return ((await response.json()) as { users: unknown[] }).users.length;
			},
			{ timeout: 30_000, message: 'the aggregator never rolled the delivered hours' }
		)
		.toBeGreaterThanOrEqual(expected);
}

let seeded: Promise<void> | null = null;

function seed(): Promise<void> {
	seeded ??= (async () => {
		await deliver([
			// 1,000 + 200 = 1,200 headline tokens; reasoning and both cache
			// classes ride beside it and are never added in.
			{
				trace: 'e1'.padEnd(32, '1'), name: 'heavy-turn', user: HEAVY, session: 'sess-heavy',
				at: HOUR_A + 10_000_000_000n,
				usage: {
					input_tokens: 1000,
					output_tokens: 200,
					reasoning_tokens: 50,
					'cache_read.input_tokens': 30,
					cache_creation_input_tokens: 10
				}
			},
			// The most recent user, with the least: last seen and tokens disagree.
			{
				trace: 'e2'.padEnd(32, '2'), name: 'light-turn', user: LIGHT, session: 'sess-light',
				at: HOUR_A + 2n * HOUR,
				usage: { input_tokens: 7, output_tokens: 3 }
			},
			// Usage that names no class the store counts: no tokens, not zero.
			{
				trace: 'e3'.padEnd(32, '3'), name: 'silent-turn', user: NONE, session: 'sess-none',
				at: HOUR_A - 2n * HOUR,
				usage: { total_tokens: 99 }
			}
		]);
		await rolled(3);
	})();
	return seeded;
}

test.beforeEach(async () => {
	await seed();
});

test('the traces table shows input plus output, with every class in the tooltip', async ({
	page
}, testInfo) => {
	await signIn(page);
	await page.goto('/traces');

	const heavy = page.getByRole('row').filter({ hasText: 'heavy-turn' });
	const light = page.getByRole('row').filter({ hasText: 'light-turn' });
	const silent = page.getByRole('row').filter({ hasText: 'silent-turn' });
	if (testInfo.project.name === 'mobile') {
		// Folded under the name, with the same number (spec 006 #22).
		await expect(heavy).toContainText('1.2k tokens');
		expect(await tooltipOver(heavy.getByText('1.2k tokens'))).toMatch(
			/^Input 1,000\nOutput 200\nCache read 30\nReasoning 50\nCache write 10$/
		);
		await expect(silent).not.toContainText('tokens');
		return;
	}
	await expect(page.getByRole('columnheader', { name: 'Tokens' })).toBeVisible();
	// 1,000 + 200: reasoning, cache read and cache write are not in it.
	const cell = heavy.getByRole('cell', { name: '1.2k' });
	await expect(cell).toBeVisible();
	await expect(cell).toHaveAttribute('title', /Input 1,000\nOutput 200\nCache read 30\nReasoning 50\nCache write 10/);
	await expect(light.getByRole('cell', { name: '10', exact: true })).toBeVisible();
	// A trace with no class is a dash with no tooltip, not a zero: the Tokens
	// cell itself, found by its header.
	const none = await tokensCell(page, silent);
	await expect(none).toHaveText('—');
	await expect(none).not.toHaveAttribute('title', /.+/);
});

test('Min tokens keeps the traces at or above the number, and is in the URL', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?min_tokens=100');

	await expect(page.getByText('heavy-turn')).toBeVisible();
	await expect(page.getByText('light-turn')).toHaveCount(0);
	// A trace with no tokens never matches, not even at zero.
	await expect(page.getByText('silent-turn')).toHaveCount(0);
});

test('the sessions table has the column, summed over the session', async ({ page }, testInfo) => {
	await signIn(page);
	await page.goto('/sessions');

	const row = page.getByRole('row').filter({ hasText: 'sess-heavy' });
	if (testInfo.project.name === 'mobile') {
		await expect(row).toContainText('1.2k tokens');
		expect(await tooltipOver(row.getByText('1.2k tokens'))).toMatch(/Reasoning 50/);
		return;
	}
	await expect(page.getByRole('columnheader', { name: 'Tokens' })).toBeVisible();
	await expect(row.getByRole('cell', { name: '1.2k' })).toHaveAttribute('title', /Reasoning 50/);
});

test('the users sort offers Tokens and puts the heaviest first', async ({ page }, testInfo) => {
	await signIn(page);
	await page.goto('/users');
	// By last seen, the light user is the more recent.
	const ids = page.locator('tbody tr td:first-child');
	await expect(ids.first()).toContainText(LIGHT);

	await page.getByLabel('Sort by').selectOption('tokens');
	await expect(page).toHaveURL(/sort=tokens/);
	await expect(ids.first()).toContainText(HEAVY);
	await expect(ids.nth(1)).toContainText(LIGHT);
	// A user with no tokens sorts last, as 0.
	await expect(ids.nth(2)).toContainText(NONE);

	if (testInfo.project.name !== 'mobile') {
		await expect(page.getByRole('columnheader', { name: 'Tokens' })).toBeVisible();
		const row = page.getByRole('row').filter({ hasText: HEAVY });
		await expect(row.getByRole('cell', { name: '1.2k' })).toHaveAttribute('title', /Cache write 10/);
	}
});

test('the user page breakdowns carry the Tokens column', async ({ page }, testInfo) => {
	await signIn(page);
	await page.goto(`/users/${encodeURIComponent(HEAVY)}?${WINDOW}`);

	const models = page.getByRole('table').filter({ has: page.getByText('Model') });
	const row = models.getByRole('row').filter({ hasText: 'tokens-model' });
	if (testInfo.project.name !== 'mobile') {
		await expect(models.getByRole('columnheader', { name: 'Tokens' })).toBeVisible();
	}
	await expect(row).toContainText('1,200');
	const environments = page.getByRole('table').filter({ has: page.getByText('Environment') });
	if (testInfo.project.name !== 'mobile') {
		await expect(environments.getByRole('columnheader', { name: 'Tokens' })).toBeVisible();
	}
});

test('the dashboard names reasoning and cache write, in the legend and the tooltip', async ({
	page
}, testInfo) => {
	await signIn(page);
	await page.goto(`/dashboard?${WINDOW}&group_by=day`);

	const legend = page
		.locator('.uplot', { has: page.locator('.u-title', { hasText: 'Tokens' }) })
		.locator('.u-legend');
	for (const label of ['Input', 'Output', 'Cache read', 'Reasoning', 'Cache write']) {
		await expect(legend).toContainText(label);
	}

	const models = page.getByRole('table').filter({ has: page.getByText('Model') });
	const row = models.getByRole('row').filter({ hasText: 'tokens-model' });
	// The headline is input plus output; reasoning is only in the tooltip.
	if (testInfo.project.name === 'mobile') {
		expect(await tooltipOver(row.getByText('1,210 tokens'))).toMatch(/Reasoning 50\nCache write 10/);
		return;
	}
	await expect(row.getByRole('cell', { name: '1,210' })).toHaveAttribute(
		'title',
		/Reasoning 50\nCache write 10/
	);
});

test('a legend toggle survives the chart being rebuilt', async ({ page }) => {
	await signIn(page);
	await page.goto(`/dashboard?${WINDOW}&group_by=day`);

	const legend = page
		.locator('.uplot', { has: page.locator('.u-title', { hasText: 'Tokens' }) })
		.locator('.u-legend');
	const reasoning = legend.locator('.u-series', { hasText: 'Reasoning' });
	// Hidden until clicked (spec 049 #20)…
	await expect(reasoning).toHaveClass(/u-off/);
	await reasoning.click();
	await expect(reasoning).not.toHaveClass(/u-off/);

	// …and a rebuild — here the system scheme flipping, which redraws every
	// chart — does not hide it again.
	const before = await legend.elementHandle();
	await page.emulateMedia({ colorScheme: 'dark' });
	// The legend on screen is a new one: the chart really was rebuilt.
	await expect.poll(() => before!.evaluate((node) => node.isConnected)).toBe(false);
	await expect(legend.locator('.u-series', { hasText: 'Input' })).toBeVisible();
	await expect(legend.locator('.u-series', { hasText: 'Reasoning' })).not.toHaveClass(/u-off/);
});
