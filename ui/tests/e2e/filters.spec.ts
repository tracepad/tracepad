import { expect, type Page } from '@playwright/test';
import { test } from './fixtures';
import { createProject, signIn as enter, state, type Account } from './harness';

// The filter panel's facet lists, against the real binary (spec 027, Testing —
// e2e): open the panel, tick two environments, apply, and read the URL, the
// rows and the chip.
//
// In a project of its own (spec 016 #18): the lists are about *which* values
// exist, so the default corpus's own environments and names would be part of
// every assertion.

/**
 * Two hours ago, on the hour: recent enough to sit inside `/facets`' own
 * default window, so no URL here carries a range and the bar stays the width
 * `wire.spec.ts` opens the panel at. Relative to now rather than a fixed
 * instant, so the suite does not quietly stop covering this in a month's time;
 * on the hour, so two Playwright projects running the same file agree.
 */
const HOUR = (() => {
	const hour = 3_600_000_000_000n;
	return (BigInt(Date.now()) * 1_000_000n / hour - 2n) * hour;
})();

let own: ReturnType<typeof createProject> | null = null;
const project = () => (own ??= createProject('filters'));

async function signIn(page: Page, into?: Account) {
	await enter(page, into ?? (await project()).account);
}

// --- a minimal OTLP/protobuf export ------------------------------------------
//
// The same builder `quality.spec.ts` and `users.spec.ts` use.

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

type Span = { trace: string; span: string; at: bigint; name: string; environment: string };

function exportOf(spans: Span[]): Uint8Array {
	const encoded = spans.map((span) =>
		bytes(2, [
			...bytes(1, hex(span.trace)),
			...bytes(2, hex(span.span)),
			...text(5, span.name),
			...fixed64(7, span.at),
			...fixed64(8, span.at + 700_000_000n),
			...bytes(9, attribute('deployment.environment.name', span.environment)),
			// The trace's own name, which the name facet lists. Without it the
			// trace takes the root span's name, which is the same string here
			// but says so by accident rather than on purpose.
			...bytes(9, attribute('langfuse.trace.name', span.name))
		])
	);
	const resource = bytes(1, bytes(1, attribute('service.name', 'filters-e2e')));
	const scope = bytes(1, text(1, 'e2e'));
	return Uint8Array.from(bytes(1, [...resource, ...bytes(2, [...scope, ...encoded.flat()])]));
}

async function deliver(spans: Span[], into?: string) {
	const { baseURL } = state();
	const key = into ?? (await project()).key;
	const response = await fetch(`${baseURL}/v1/traces`, {
		method: 'POST',
		headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/x-protobuf' },
		body: Buffer.from(exportOf(spans))
	});
	if (!response.ok) throw new Error(`ingest: ${response.status} ${await response.text()}`);
}

/**
 * Waits until a value is on the facet list, because delivery is not the same
 * event: the export returns as soon as the batch is accepted, the writer
 * commits it after, and the rollup takes it after that. Polling rather than
 * sleeping keeps the wait honest.
 *
 * `/facets` rides spec 013's seam: past the rollup's watermark it scans the
 * live rows, and behind it, it answers from `names_hourly` and `stats_hourly`.
 * A trace delivered into an hour the aggregator has already rolled is
 * therefore in the listing at once and on the facet list only after the next
 * pass re-rolls that hour as dirty — measured at about a second with
 * `TRACEPAD_ROLLUP_INTERVAL=1s`, and longer on a loaded runner rolling every
 * project the suite's other workers are creating.
 *
 * The panel's assertion has a five-second timeout of its own, so waiting for
 * the listing instead left the case racing the aggregator rather than testing
 * what it is about.
 */
async function onTheFacetList(key: string, environment: string) {
	const { baseURL } = state();
	await expect
		.poll(
			async () => {
				const response = await fetch(`${baseURL}/api/v1/facets`, {
					headers: { Authorization: `Bearer ${key}` }
				});
				if (!response.ok) return [];
				const body = (await response.json()) as { environment?: { value: string }[] };
				return (body.environment ?? []).map((one) => one.value);
			},
			{ timeout: 15_000, message: `${environment} never reached the facet list` }
		)
		.toContain(environment);
}

const id = (prefix: string, n: number) => `${prefix}${n}`.padEnd(32, '0');
const span = (prefix: string, n: number) => `${prefix}${n}`.padEnd(16, '0');

let seeded: Promise<void> | null = null;

/**
 * The corpus this file reads, idempotent so both Playwright projects may run
 * it: three environments of different sizes — which is what makes the counts
 * worth showing — and two trace names.
 */
function seed(): Promise<void> {
	seeded ??= (async () => {
		const spans: Span[] = [];
		const add = (n: number, name: string, environment: string) =>
			spans.push({
				trace: id('e', n),
				span: span('e', n),
				at: HOUR + BigInt(n) * 1_000_000_000n,
				name,
				environment
			});
		for (let n = 1; n <= 4; n++) add(n, 'chat', 'production');
		add(5, 'summarize', 'production');
		add(6, 'chat', 'staging');
		// One trace in a typo of an environment: the case the counts exist for.
		add(7, 'summarize', 'prod');
		await deliver(spans);
	})();
	return seeded;
}

test.beforeEach(async () => {
	await seed();
});

/** The filter popover, opened. */
async function openPanel(page: Page) {
	await page.getByRole('button', { name: /^Filters/ }).click();
	await expect(page.getByRole('group', { name: 'Environment' })).toBeVisible();
}

/**
 * One box by its value. The name is the value alone, with the count read after
 * a comma, so `^production,` cannot also match `production-eu`.
 */
const box = (page: Page, group: string, value: string) =>
	page
		.getByRole('group', { name: group })
		.getByRole('checkbox', { name: new RegExp(`^${value}(,|$)`) });

test('the panel lists the environments with their counts', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces');
	await openPanel(page);

	const environments = page.getByRole('group', { name: 'Environment' });
	// Busiest first, and the count beside each — which is what tells the typo
	// `prod: 1` from `production: 5`.
	await expect(environments.getByRole('checkbox')).toHaveCount(3);
	await expect(environments).toContainText('production');
	await expect(environments).toContainText('5');
	await expect(environments).toContainText('prod');
	await expect(environments).toContainText('staging');

	// And the names, from `names_hourly` behind the watermark or from the live
	// tail before the first pass — the reader cannot tell which.
	const names = page.getByRole('group', { name: 'Name' });
	await expect(names.getByRole('checkbox')).toHaveCount(2);
	await expect(names).toContainText('chat');
	await expect(names).toContainText('summarize');
});

test('ticking two environments filters by either, and the chip reads both', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces');
	await openPanel(page);

	await box(page, 'Environment', 'staging').check();
	await box(page, 'Environment', 'prod').check();
	await page.getByRole('button', { name: 'Apply' }).click();

	// The URL carries the comma form, so the listing, the chip and the API
	// spell the filter identically (Decision 7).
	await expect(page).toHaveURL(/environment=staging%2Cprod|environment=prod%2Cstaging/);
	await expect(page.locator('tbody tr')).toHaveCount(2);
	// Two values are named in full: a glance holds two.
	await expect(page.getByRole('button', { name: /Remove filter Environment:/ })).toContainText(
		/staging/
	);
});

test('a name narrows the rows the way the API does', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces');
	await openPanel(page);

	await box(page, 'Name', 'summarize').check();
	await page.getByRole('button', { name: 'Apply' }).click();

	await expect(page).toHaveURL(/name=summarize/);
	await expect(page.locator('tbody tr')).toHaveCount(2);
});

// A URL is a document: a link to a value the range no longer holds must still
// show that it filters, and must still be undone (Decision 6).
test('a deep link with an unknown environment shows it checked and removable', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces?environment=canary');

	const chip = page.getByRole('button', { name: 'Remove filter Environment: canary' });
	await expect(chip).toBeVisible();
	await expect(page.locator('tbody tr')).toHaveCount(0);

	await openPanel(page);
	const canary = box(page, 'Environment', 'canary');
	await expect(canary).toBeChecked();
	await canary.uncheck();
	await page.getByRole('button', { name: 'Apply' }).click();

	await expect(page).not.toHaveURL(/environment=/);
	await expect(page.locator('tbody tr')).toHaveCount(7);
});

test('the sessions bar offers the same list for the environment', async ({ page }) => {
	await signIn(page);
	await page.goto('/sessions');

	await page.getByRole('button', { name: 'Environment' }).click();
	const environments = page.getByRole('group', { name: 'Environment' });
	await expect(environments.getByRole('checkbox')).toHaveCount(3);
});

test('the panel is usable at a phone width and the page never scrolls sideways', async ({
	page
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile', 'the narrow width is the test');
	await signIn(page);
	await page.goto('/traces');
	await openPanel(page);

	// The panel is whole, frame included: capped one level down it stood its
	// own border past the top edge, the way the range picker stood a row of
	// presets there.
	const panel = (await page.locator('[data-popover-content]').boundingBox())!;
	const viewport = page.viewportSize()!;
	expect(panel.y).toBeGreaterThanOrEqual(0);
	expect(panel.x).toBeGreaterThanOrEqual(0);
	expect(panel.y + panel.height).toBeLessThanOrEqual(viewport.height);
	expect(panel.x + panel.width).toBeLessThanOrEqual(viewport.width);

	await box(page, 'Environment', 'staging').check();
	await page.getByRole('button', { name: 'Apply' }).click();
	await expect(page).toHaveURL(/environment=staging/);

	const overflow = await page.evaluate(
		() => document.documentElement.scrollWidth - document.documentElement.clientWidth
	);
	expect(overflow).toBeLessThanOrEqual(0);
});

/**
 * A window written out in full rather than picked from the presets, which is
 * what a link somebody was sent carries. It is the longest label the trigger
 * ever wears, and the reason the bar had to be told what may shrink (Decision
 * 22). Ending now and forty days wide, so the corpus is inside it.
 */
function wideWindow(): string {
	const now = Date.now();
	return new URLSearchParams({
		from: new Date(now - 40 * 86_400_000).toISOString(),
		to: new Date(now).toISOString()
	}).toString();
}

/**
 * The pin for Decision 22. Before it, the bar's controls were laid out at the
 * width they wanted and painted outside the box they were given: *Add to
 * queue…* landed on top of *Filters* and swallowed its clicks, so the panel
 * could not be opened at all on a phone whose URL carried a window.
 *
 * `elementFromPoint` rather than the click alone, because the click's failure
 * is a thirty-second timeout that reads like a slow page.
 */
for (const width of [320, 375]) {
	test(`a long window label leaves the Filters button clickable at ${width} px`, async ({
		page
	}, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile', 'the narrow width is the test');
		await page.setViewportSize({ width, height: 812 });
		await signIn(page);
		await page.goto(`/traces?${wideWindow()}`);

		const trigger = page.getByRole('button', { name: /^Filters/ });
		const covered = await trigger.evaluate((button) => {
			const box = button.getBoundingClientRect();
			const at = document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2);
			return { hit: !!at && button.contains(at), right: box.right, viewport: window.innerWidth };
		});
		expect(covered.hit).toBe(true);
		expect(covered.right).toBeLessThanOrEqual(covered.viewport);

		// Since Decision 23 the search box is on a row of its own, so the window
		// is laid out on the bar's whole width: the longest label it wears, 271 px
		// at 320, fits and is shown whole at both widths (measured), and *Filters*
		// wraps beneath it when the two do not fit together rather than the window
		// being cut. The shrink rule of #22 is what the Sessions bar's case below
		// still pins.
		const range = page.getByRole('button', { name: /^Time range: / });
		await expect(range).toHaveAccessibleName(/→/);
		expect(await range.locator('span').evaluate((el) => el.scrollWidth <= el.clientWidth)).toBe(true);

		await openPanel(page);
	});
}

/**
 * Decision 23: on a phone the search box has a row of its own. Beside the
 * window and *Filters* it was left 80 px at 375 — a placeholder cut to nothing
 * and a field that showed two letters of what was typed. Measured at the two
 * widths the narrowest phones come in, at two larger phones, and one under
 * `sm` — where the bar must fill its row rather than the width of what it
 * holds: the field is as wide as the bar, nothing the bar holds crosses
 * anything else, and nothing is outside the window — with the plain bar, with
 * the longest label the window wears, and with a search and a chip on it.
 */
for (const width of [320, 375, 414, 600, 639]) {
	for (const [name, query] of [
		['the plain bar', ''],
		['a window written out', wideWindow()],
		['a search and a chip', 'q=password&environment=staging']
	] as const) {
		test(`the search box is a row of its own at ${width} px with ${name}`, async ({
			page
		}, testInfo) => {
			test.skip(testInfo.project.name !== 'mobile', 'the narrow width is the test');
			await page.setViewportSize({ width, height: 812 });
			await signIn(page);
			await page.goto(`/traces${query && `?${query}`}`);

			// A bare path is redirected to the project's on the client (spec 029
			// #3), and `goto` can return before that lands: the page is empty for
			// a few tens of milliseconds, and `all()` below does not wait. So wait
			// for the address and for every part this case must measure, rather
			// than measure whatever happened to be drawn.
			await expect(page).toHaveURL(/\/p\/[0-9a-f]{32}\/traces(\?|$)/);
			const search = page.getByRole('searchbox', { name: /^Search/ });
			await expect(search).toBeVisible();
			await expect(page.getByRole('button', { name: /^Filters/ })).toBeVisible();
			if (name === 'a window written out')
				await expect(page.getByRole('button', { name: /^Time range: / })).toHaveAccessibleName(/→/);
			if (query.includes('environment='))
				await expect(page.getByRole('button', { name: /^Remove filter / })).toBeVisible();

			const parts = [
				search,
				page.getByRole('button', { name: /^Time range: / }),
				page.getByRole('button', { name: /^Filters/ }),
				page.getByRole('button', { name: /^Add to queue/ }),
				page.getByRole('button', { name: /^Remove filter / })
			];
			const boxes: { who: number; x: number; y: number; w: number; h: number }[] = [];
			for (const [who, part] of parts.entries()) {
				for (const one of await part.all()) {
					const b = await one.boundingBox();
					if (b) boxes.push({ who, x: b.x, y: b.y, w: b.width, h: b.height });
				}
			}
			expect(boxes.some((b) => b.who === 2)).toBe(true);

			// A field you can type into: the bar's width, less its padding.
			const field = boxes.find((b) => b.who === 0);
			if (!field) throw new Error('the search box was not measured');
			expect(field.w).toBeGreaterThanOrEqual(width - 40);

			for (const b of boxes) {
				expect(b.x).toBeGreaterThanOrEqual(0);
				expect(b.x + b.w).toBeLessThanOrEqual(width);
			}
			for (const [i, a] of boxes.entries()) {
				for (const b of boxes.slice(i + 1)) {
					const apart =
						a.x + a.w <= b.x + 0.5 ||
						b.x + b.w <= a.x + 0.5 ||
						a.y + a.h <= b.y + 0.5 ||
						b.y + b.h <= a.y + 0.5;
					expect(apart, `${a.who} and ${b.who} cross`).toBe(true);
				}
			}

			// Its own row: everything else on the bar is below it.
			for (const b of boxes.filter((b) => b.who !== 0 && b.who !== 3)) {
				expect(b.y).toBeGreaterThanOrEqual(field.y + field.h - 0.5);
			}

			const overflow = await page.evaluate(
				() => document.documentElement.scrollWidth - document.documentElement.clientWidth
			);
			expect(overflow).toBeLessThanOrEqual(0);

			// Typed into, it holds what was typed and is still inside the window.
			await search.fill('a long question about a refund');
			await expect(search).toHaveValue('a long question about a refund');
			const typed = (await search.boundingBox())!;
			expect(typed.x + typed.width).toBeLessThanOrEqual(width);

			await testInfo.attach(`bar-${width}-${name}`, {
				body: await page.screenshot({ clip: { x: 0, y: 0, width, height: 260 } }),
				contentType: 'image/png'
			});
		});
	}
}

/** The same bar, one screen over (Decision 22): the same three answers. */
test('the sessions bar keeps its controls inside it at a phone width', async ({
	page
}, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile', 'the narrow width is the test');
	await signIn(page);
	await page.goto(`/sessions?${wideWindow()}`);

	const trigger = page.getByRole('button', { name: 'Environment' });
	const hit = await trigger.evaluate((button) => {
		const box = button.getBoundingClientRect();
		const at = document.elementFromPoint(box.x + box.width / 2, box.y + box.height / 2);
		return !!at && button.contains(at);
	});
	expect(hit).toBe(true);

	const range = page.getByRole('button', { name: /^Time range: / });
	expect(await range.locator('span').evaluate((el) => el.scrollWidth > el.clientWidth)).toBe(true);

	await trigger.click();
	await expect(page.getByRole('group', { name: 'Environment' })).toBeVisible();
});

/**
 * The other side of Decision 22: the window control is shared by five bars,
 * and only the two that pass it a minimum may make it narrow. On the dashboard and
 * Quality the field and the bucket buttons beside it cannot shrink at all, so
 * a picker allowed to shrink there absorbs the whole deficit — for one commit
 * on this branch it was 34 px wide with **no label at all**, on the default
 * window, which is not even a narrow one. Those two bars scroll instead, which
 * is what they have always done.
 */
for (const screen of ['/dashboard', '/quality'] as const) {
	test(`the window keeps its label on ${screen} at a phone width`, async ({ page }, testInfo) => {
		test.skip(testInfo.project.name !== 'mobile', 'the narrow width is the test');
		await signIn(page);
		await page.goto(screen);

		const label = page.getByRole('button', { name: /^Time range: / }).locator('span');
		const shown = await label.evaluate((el) => ({
			width: el.getBoundingClientRect().width,
			text: el.textContent?.trim() ?? ''
		}));
		expect(shown.text).not.toBe('');
		expect(shown.width).toBeGreaterThan(0);
	});
}

// What the live tail is for (Decision 3), and why a closed panel forgets what
// it read (Decision 15): an environment first seen a minute ago is on the list
// the next time the panel opens, without a reload. The window did not move —
// on the listing's default range there is no `from` or `to` in the URL at all
// — so a memory that survived the closing would freeze the lists for the life
// of the page.
//
// In a project of its own, because this one ingests while a page is open and
// every other case here counts the shared corpus.
test('an environment first seen after the panel closed is on the list when it reopens', async ({
	page
}) => {
	const fresh = await createProject('facet-reopen');
	await deliver(
		[{ trace: id('f', 1), span: span('f', 1), at: HOUR, name: 'chat', environment: 'production' }],
		fresh.key
	);
	await onTheFacetList(fresh.key, 'production');
	await signIn(page, fresh.account);
	await page.goto('/traces');

	await openPanel(page);
	await expect(box(page, 'Environment', 'production')).toBeVisible();
	await expect(box(page, 'Environment', 'canary')).toHaveCount(0);

	await page.keyboard.press('Escape');
	await expect(page.getByRole('group', { name: 'Environment' })).toBeHidden();

	await deliver(
		[
			{
				trace: id('f', 2),
				span: span('f', 2),
				at: HOUR + 1_000_000_000n,
				name: 'chat',
				environment: 'canary'
			}
		],
		fresh.key
	);
	// The point of the case is what the *reopening* reads, so `canary` has to
	// be on the facet list before the panel opens again: a panel that opened
	// too early would read a list without it and — correctly — not read it
	// again until the next opening.
	await onTheFacetList(fresh.key, 'canary');

	await openPanel(page);
	await expect(box(page, 'Environment', 'canary')).toBeVisible();
});
