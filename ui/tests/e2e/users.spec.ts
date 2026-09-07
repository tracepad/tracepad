import { expect, test, type Page } from '@playwright/test';
import { createProject, state } from './harness';

// The Users screens (spec 023, Testing — e2e), against the real binary.
//
// In a project of its own (spec 016 #18): the listing answers from the rollup,
// so the assertions are about *which* users are in it, and the default
// corpus's own users would move them. The traffic is exported here as OTLP
// protobuf, the way an SDK exports it — two users with different traffic,
// cost and activity, so the four sorts have something to disagree about.

const ALICE = 'alice@e2e';
const BOB = 'bob@e2e';
/** Two hours in the past, so both are closed from the aggregator's first pass. */
const HOUR_A = 1787738400000000000n; // 2026-08-26T10:00:00Z
const HOUR_B = 1787745600000000000n; // 2026-08-26T12:00:00Z

let own: ReturnType<typeof createProject> | null = null;
const project = () => (own ??= createProject('users'));

async function signIn(page: Page) {
	const { key } = await project();
	await page.goto('/login');
	await page.getByLabel('Project key').fill(key);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await expect(page).toHaveURL(/\/traces$/);
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
	// A window wide enough to hold the fixtures, which are fixed in the past.
	await page.goto(
		`/users/${encodeURIComponent(ALICE)}?from=2026-08-01T00:00:00Z&to=2026-09-30T00:00:00Z`
	);

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
		'/traces?user_id=nobody-at-all'
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
			at: HOUR_A + 30_000_000_000n,
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
