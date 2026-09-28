import { expect, test, type Page } from '@playwright/test';
import { createProject, sideways, signIn as enter, state } from './harness';

// The Users screens (spec 023, Testing — e2e), against the real binary.
//
// In a project of its own (spec 016 #18): the listing answers from the rollup,
// so the assertions are about *which* users are in it, and the default
// corpus's own users would move them. The traffic is exported here as OTLP
// protobuf, the way an SDK exports it — two users with different traffic,
// cost and activity, so the four sorts have something to disagree about.

const ALICE = 'alice@e2e';
const BOB = 'bob@e2e';
/**
 * Two hours in the past, so both are closed from the aggregator's first pass —
 * and relative to now, on the hour, as `filters.spec.ts` does: the page opens
 * on the last thirty days, and a fixed instant fell out of that window a month
 * after it was written, taking the charts with it.
 */
const HOUR = 3_600_000_000_000n;
const HOUR_A = (BigInt(Date.now()) * 1_000_000n / HOUR - 6n) * HOUR;
const HOUR_B = HOUR_A + 2n * HOUR;
/** A window around both hours, for a test that names one. */
const iso = (nanos: bigint) => new Date(Number(nanos / 1_000_000n)).toISOString();
const WINDOW = `from=${iso(HOUR_A - 24n * HOUR)}&to=${iso(HOUR_B + 24n * HOUR)}`;

/**
 * Now, in nanoseconds — where a test puts a trace it wants to read back
 * *without* waiting for a pass.
 *
 * The hour in progress is never rolled (an hour is closed one interval after it
 * ends), so the watermark cannot reach it and `GET /api/v1/users/{id}` answers
 * it from the live tail. A trace dropped into one of the fixed hours above has
 * no such guarantee: that hour is already behind the watermark, so the trace is
 * in the rollup's half of the seam but not in the rollup until the pass that
 * re-rolls the hour it made dirty — a race, and the reason this exists.
 */
const nowNanos = () => BigInt(Date.now()) * 1_000_000n;

let own: ReturnType<typeof createProject> | null = null;
const project = () => (own ??= createProject('users'));

async function signIn(page: Page) {
	await enter(page, (await project()).account);
}

// --- a minimal OTLP/protobuf export ------------------------------------------
//
// The same builder `evals.spec.ts` uses, with a double-valued attribute added:
// the cost is what makes `sort=cost` order the listing differently from
// `sort=last_seen`, which is the assertion.

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
const double = (num: number, value: number) => {
	const buffer = Buffer.alloc(8);
	buffer.writeDoubleLE(value);
	return field(num, 1, [...buffer]);
};
const hex = (value: string) => [...Buffer.from(value, 'hex')];
/** KeyValue{key, value: AnyValue{string_value}}. */
const attribute = (key: string, value: string) => [...text(1, key), ...bytes(2, text(1, value))];
/** The same, with AnyValue{double_value} — field 4 of AnyValue. */
const money = (key: string, value: number) => [...text(1, key), ...bytes(2, double(4, value))];

type Span = {
	trace: string;
	user: string;
	session: string;
	at: bigint;
	cost: number;
	environment: string;
};

function exportOf(spans: Span[]): Uint8Array {
	const encoded = spans.map((span) =>
		bytes(2, [
			...bytes(1, hex(span.trace)),
			...bytes(2, hex(span.trace.slice(0, 16))),
			...text(5, 'answer'),
			...fixed64(7, span.at),
			...fixed64(8, span.at + 700_000_000n),
			...bytes(9, attribute('user.id', span.user)),
			...bytes(9, attribute('session.id', span.session)),
			...bytes(9, attribute('gen_ai.request.model', 'claude-sonnet-5')),
			...bytes(9, attribute('deployment.environment.name', span.environment)),
			...bytes(9, money('gen_ai.usage.cost', span.cost))
		])
	);
	const resource = bytes(1, bytes(1, attribute('service.name', 'users-e2e')));
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

/**
 * Waits for the aggregator to take the delivered hours. The listing is the
 * rollup and nothing else (spec 023 #4), so this wait *is* the lag the docs
 * publish — polling the endpoint rather than sleeping keeps it honest.
 */
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

/** The corpus this file reads: idempotent, so both Playwright projects may run it. */
function seed(): Promise<void> {
	seeded ??= (async () => {
		await deliver([
			// Alice: two traces in one session, in the earlier hour, expensive.
			{ trace: 'a0'.padEnd(32, '1'), user: ALICE, session: 'sess-a', at: HOUR_A + 10_000_000_000n, cost: 0.4, environment: 'production' },
			{ trace: 'a1'.padEnd(32, '2'), user: ALICE, session: 'sess-a', at: HOUR_A + 20_000_000_000n, cost: 0.4, environment: 'staging' },
			// Bob: one trace, later, cheap — so last seen and cost disagree.
			{ trace: 'b0'.padEnd(32, '3'), user: BOB, session: 'sess-b', at: HOUR_B + 10_000_000_000n, cost: 0.001, environment: 'production' }
		]);
		await rolled(2);
	})();
	return seeded;
}

test.beforeEach(async () => {
	await seed();
});

test('the listing shows both users, sorted by last seen', async ({ page }) => {
	await signIn(page);
	await page.getByRole('link', { name: 'Users' }).click();

	await expect(page).toHaveURL(/\/users$/);
	// Bob is the more recent, so he is first under the default sort.
	const ids = page.locator('tbody tr td:first-child');
	await expect(ids.first()).toContainText('bob@e2e');
	await expect(ids.nth(1)).toContainText('alice@e2e');
	// Every number counts traces, and alice's two are in one session.
	const alice = page.getByRole('row').filter({ hasText: 'alice@e2e' });
	await expect(alice).toContainText('2');
});

test('sorting by cost reorders and stays in the URL', async ({ page }) => {
	await signIn(page);
	await page.goto('/users');
	await page.getByLabel('Sort by').selectOption('cost');

	await expect(page).toHaveURL(/sort=cost/);
	const ids = page.locator('tbody tr td:first-child');
	await expect(ids.first()).toContainText('alice@e2e');
	await expect(ids.nth(1)).toContainText('bob@e2e');
});

test('a prefix narrows the listing', async ({ page }) => {
	await signIn(page);
	await page.goto('/users?prefix=alice');

	await expect(page.getByText('alice@e2e').first()).toBeVisible();
	await expect(page.getByText('bob@e2e')).toHaveCount(0);
	// The prefix is in the URL, so this is a link somebody can send.
	await expect(page.getByPlaceholder('User id starts with')).toHaveValue('alice');
});

test('the user page draws the cards, the charts, the breakdowns and both tabs', async ({
	page
}) => {
	await signIn(page);
	// A window named in the URL, wide enough to hold the fixtures.
	await page.goto(`/users/${encodeURIComponent(ALICE)}?${WINDOW}`);

	// The cards are `GET /api/v1/users/{id}`.
	for (const label of ['Traces', 'Sessions', 'With errors', 'Cost', 'p50', 'Last seen']) {
		await expect(page.locator('dt').filter({ hasText: new RegExp(`^${label}$`) })).toBeVisible();
	}
	// Two charts, drawn rather than merely titled.
	await expect(page.locator('.uplot canvas')).toHaveCount(2);
	await expect(page.locator('.u-legend', { hasText: 'Sessions' })).toBeVisible();

	// Both breakdowns, over `?user_id=`: alice ran in two environments.
	const environments = page.getByRole('table').filter({ has: page.getByText('Environment') });
	await expect(environments.getByRole('row').filter({ hasText: 'production' })).toBeVisible();
	await expect(environments.getByRole('row').filter({ hasText: 'staging' })).toBeVisible();
	await expect(
		page.getByRole('table').filter({ has: page.getByText('Model') }).getByRole('row').filter({ hasText: 'claude-sonnet-5' })
	).toBeVisible();

	// The sessions tab is the default; the traces tab is one click away.
	await expect(page.getByText('sess-a').first()).toBeVisible();
	await page.getByRole('button', { name: 'traces' }).click();
	await expect(page).toHaveURL(/tab=traces/);
	await expect(page.getByText('answer').first()).toBeVisible();
});

// The page's URL carries two listings' cursors and the panel's key beside its
// own window, so the effect behind the header and the charts has to depend on
// what it actually asks — not on an object rebuilt whenever any of that moves.
// It did, and turning a page in a tab re-fetched the summary, the timeline and
// both breakdowns (found in the second review of PR #42).
test('turning a page in a tab does not re-ask for the charts', async ({ page }) => {
	await signIn(page);
	// `limit=1` so alice's two traces are two pages.
	await page.goto(`/users/${encodeURIComponent(ALICE)}?tab=traces&limit=1`);
	await expect(page.locator('.uplot canvas').first()).toBeVisible();

	const asked: string[] = [];
	page.on('request', (request) => {
		const url = new URL(request.url());
		if (url.pathname.startsWith('/api/v1/')) asked.push(url.pathname);
	});

	await page.getByRole('button', { name: 'Next page' }).click();
	await expect(page).toHaveURL(/cursor=/);
	// Give anything the turn would have triggered time to go out.
	await expect.poll(() => asked.length, { timeout: 3000 }).toBeGreaterThan(0);
	await page.waitForTimeout(500);

	// The turn is one request: the tab's own listing.
	expect(asked.filter((path) => path.startsWith('/api/v1/stats'))).toEqual([]);
	expect(asked.filter((path) => path.startsWith('/api/v1/users/'))).toEqual([]);
	expect(asked).toContain('/api/v1/traces');
});

test('a user id in the traces table links to the page', async ({ page }) => {
	await signIn(page);
	await page.goto(`/traces?user_id=${encodeURIComponent(BOB)}`);

	// The cell's own link, which stops the row's click from opening the peek
	// panel over it (spec 023, Application contract).
	await page.getByRole('link', { name: BOB, exact: true }).first().click();
	await expect(page).toHaveURL(new RegExp(`/users/${encodeURIComponent(BOB).replace('@', '%40')}`));
	await expect(page.locator('dt').filter({ hasText: /^Sessions$/ })).toBeVisible();
});

test('an unknown id says so and points at the traces filter', async ({ page }) => {
	await signIn(page);
	await page.goto('/users/nobody-at-all');

	await expect(page.getByText('Nothing is filed under')).toBeVisible();
	await expect(page.getByRole('link', { name: 'traces filtered by it' })).toHaveAttribute(
		'href',
		/^\/p\/[0-9a-f]{32}\/traces\?user_id=nobody-at-all$/
	);
});

// A user id is whatever the application set, and applications set odd things.
// The route decoded its param a second time, which threw `URIError` inside a
// `$derived` on a bare `%` — the page did not render at all — and quietly
// resolved a literal `%2F` to a different id (found in review of PR #42).
test('a user id carrying a per-cent sign is read as it stands', async ({ page }) => {
	const odd = '50%-off@e2e';
	await deliver([
		{
			trace: 'cc'.padEnd(32, '7'),
			user: odd,
			session: 'sess-odd',
			at: nowNanos(),
			cost: 0.002,
			environment: 'production'
		}
	]);

	await signIn(page);
	// The live tail answers without a pass, so this is the page proper and
	// not the not-found state.
	await page.goto(`/users/${encodeURIComponent(odd)}`);

	await expect(page.locator('dt').filter({ hasText: /^Traces$/ })).toBeVisible();
	await expect(page.getByTitle(odd).first()).toBeVisible();
	// And the id survived the round trip into the tab's filter.
	await expect(page.getByRole('link', { name: /Open the full sessions listing/ })).toHaveAttribute(
		'href',
		new RegExp(`^/p/[0-9a-f]{32}/sessions\\?user_id=${encodeURIComponent(odd)}$`)
	);
});

test('erasing a user shows the dry run, refuses a wrong echo, and lands on /users', async ({
	page
}) => {
	// A user of this test's own, so the assertions above keep their corpus.
	const victim = `erase-${Math.random().toString(36).slice(2, 8)}@e2e`;
	await deliver([
		{
			trace: Math.random().toString(16).slice(2, 10).padEnd(32, 'f'),
			user: victim,
			session: `sess-${victim}`,
			at: nowNanos(),
			cost: 0.01,
			environment: 'production'
		}
	]);

	await signIn(page);
	// No pass is needed: the user page merges the live tail (spec 023 #4).
	await page.goto(`/users/${encodeURIComponent(victim)}`);
	await page.getByRole('button', { name: 'Erase data' }).click();
	await page.getByRole('button', { name: 'Show what would go' }).click();

	// The server's own dry run, not a count the browser guessed.
	await expect(page.getByText('This would delete')).toBeVisible();
	const echo = page.getByLabel(/Type the user id to confirm/);
	await echo.fill('not-the-id');
	await expect(page.getByRole('button', { name: 'Erase this user’s data' })).toBeDisabled();

	await echo.fill(victim);
	await page.getByRole('button', { name: 'Erase this user’s data' }).click();

	await expect(page).toHaveURL(/\/users$/);
	await expect(page.getByText(victim)).toHaveCount(0);
});

test('375 px never scrolls the page sideways', async ({ page }) => {
	await signIn(page);
	const overflow = async () =>
		page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);

	await page.setViewportSize({ width: 375, height: 812 });
	await page.goto('/users');
	await expect(page.getByText('alice@e2e').first()).toBeVisible();
	expect(await overflow()).toBeLessThanOrEqual(0);

	await page.goto(`/users/${encodeURIComponent(ALICE)}`);
	await expect(page.locator('dt').filter({ hasText: /^Traces$/ })).toBeVisible();
	expect(await overflow()).toBeLessThanOrEqual(0);
});

// Spec 006 #22: a listing folds by the width of its own box, not the screen's.
// On a phone the user and the errors stay and the rest folds under the id; in
// a desktop window of 1,000 px the column leaves the table 792 px, which the
// users' seven columns do not fit and the sessions' six do.
test('the users fold on a phone and in a narrow desktop window', async ({ page }, testInfo) => {
	if (testInfo.project.name === 'desktop') await page.setViewportSize({ width: 1000, height: 800 });
	await signIn(page);
	await page.goto('/users');

	const table = page.locator('main table');
	await expect(table.locator('thead th')).toHaveText(['User', 'Errors']);
	const alice = page.getByRole('row').filter({ hasText: 'alice@e2e' });
	await expect(alice).toContainText('2 traces · 1 session · $0.8000');
	expect(await sideways(table)).toBeLessThanOrEqual(0);

	if (testInfo.project.name !== 'desktop') return;
	await page.goto('/sessions');
	await expect(page.locator('main table thead th')).toHaveCount(6);
});
