import { expect, test, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { createProject, state } from './harness';

// The Evals screens (spec 016, Testing — e2e), against the real binary. The
// corpus is not enough here: the suite creates a dataset, its items and a run
// with the client-supplied id fixture 009 stamps, delivers the fixture before
// the run exists and again after it, so the link resolves on re-delivery
// (spec 002 #6), and exports a second run — the comparison needs one — the
// way a harness exports: a minimal OTLP request built here.
//
// In a project of its own (spec 016 #18), not the default one: the pagination
// and stats suites count the default corpus, and a second run's traces would
// move their numbers. Every write is idempotent all the same (spec 014 #6,
// #9; scores by id), because both Playwright projects run this file.

/** The run fixture 009 stamps, and the two cases it names (docs/datasets.md). */
const RUN_A = '0e5a7c1d2b3f4a6980c1d2e3f4a5b6c7';
const RUN_B = '1f6b7c1d2b3f4a6980c1d2e3f4a5b6c8';
const ITEM_1 = 'a1b2c3d4e5f60718293a4b5c6d7e8f90';
const ITEM_2 = 'b2c3d4e5f60718293a4b5c6d7e8f90a1';
const DATASET = 'support-golden';
/** Fixture 009's three traces of run A: two at item 1, one at item 2. */
const A_TRACES = [
	['e0a1b2c3d4e5f60718293a4b5c6d7e8f', ITEM_1, 0.5],
	['e2c3d4e5f60718293a4b5c6d7e8f90a1', ITEM_1, 1],
	['e1b2c3d4e5f60718293a4b5c6d7e8f90', ITEM_2, 1]
] as const;
/** Run B's two traces, one per case, exported below. */
const B_TRACES = [
	['b0a1b2c3d4e5f60718293a4b5c6d7e8f', ITEM_1, 1],
	['b1b2c3d4e5f60718293a4b5c6d7e8f90', ITEM_2, 1]
] as const;

/** The suite's own project, minted once per worker. */
let own: Promise<{ key: string }> | null = null;
const project = () => (own ??= createProject('evals'));

/** Signs in with the project's key by hand, the way a second project is opened. */
async function signIn(page: Page) {
	const { key } = await project();
	await page.goto('/login');
	await page.getByLabel('Project key').fill(key);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await expect(page).toHaveURL(/\/traces$/);
}

async function call(method: string, path: string, body?: unknown, type = 'application/json') {
	const { baseURL } = state();
	const { key } = await project();
	const response = await fetch(`${baseURL}${path}`, {
		method,
		headers: { Authorization: `Bearer ${key}`, 'Content-Type': type },
		body:
			body === undefined
				? undefined
				: body instanceof Uint8Array
					? Buffer.from(body)
					: JSON.stringify(body)
	});
	return response;
}

async function must(method: string, path: string, body?: unknown, allowed: number[] = []) {
	const response = await call(method, path, body);
	if (!response.ok && !allowed.includes(response.status)) {
		throw new Error(`${method} ${path}: ${response.status} ${await response.text()}`);
	}
}

// --- a minimal OTLP/protobuf export ------------------------------------------
//
// Just enough of the wire format for two spans with two attributes each: the
// shape a harness exports (docs/datasets.md), without an SDK in the suite.

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
/** KeyValue{key, value: AnyValue{string_value}}. */
const attribute = (key: string, value: string) => [...text(1, key), ...bytes(2, text(1, value))];

/** ExportTraceServiceRequest with one span per trace, stamped for the run. */
function exportOf(run: string, traces: readonly (readonly [string, string, number])[]): Uint8Array {
	const start = 1787738500000000000n;
	const spans = traces.flatMap(([traceID, itemID], i) => {
		const at = start + BigInt(i) * 1_000_000_000n;
		return bytes(2, [
			...bytes(1, hex(traceID)),
			...bytes(2, hex(traceID.slice(0, 16))),
			...text(5, 'answer-case'),
			...fixed64(7, at),
			...fixed64(8, at + 700_000_000n),
			...bytes(9, attribute('tracepad.run_id', run)),
			...bytes(9, attribute('tracepad.item_id', itemID))
		]);
	});
	const resource = bytes(1, bytes(1, attribute('service.name', 'support-eval')));
	const scope = bytes(1, text(1, 'e2e'));
	return Uint8Array.from(bytes(1, [...resource, ...bytes(2, [...scope, ...spans])]));
}

let seeded: Promise<void> | null = null;

/** The corpus this file reads: run once per worker, idempotent on the server. */
function seed(): Promise<void> {
	seeded ??= (async () => {
		// The fixture first, before the run it names exists: an orphan delivery
		// (spec 014 #3) that the re-delivery below has to catch up with.
		const fixture = readFileSync(join(resolve(process.cwd(), '..'), 'testdata', 'otlp', '009-eval-run.pb'));
		const orphan = await call('POST', '/v1/traces', Uint8Array.from(fixture), 'application/x-protobuf');
		if (!orphan.ok) throw new Error(`post fixture 009: ${orphan.status}`);
		await must('PUT', '/api/v1/score-configs/accuracy', {
			data_type: 'numeric',
			direction: 'higher',
			min: 0,
			max: 1
		});
		await must('POST', `/api/v1/datasets/${DATASET}/items`, [
			{
				id: ITEM_1,
				input: { question: 'which plan includes priority routing?' },
				expected_output: { answer: 'The Team plan and above.' }
			},
			{ id: ITEM_2, input: { question: 'can I get a refund?' }, expected_output: { answer: 'Within 30 days.' } }
		]);
		await must('POST', `/api/v1/datasets/${DATASET}/runs`, { id: RUN_A, name: 'prompt v7' });
		// The fixture again, now that the run it names exists (spec 002 #6).
		const posted = await call('POST', '/v1/traces', Uint8Array.from(fixture), 'application/x-protobuf');
		if (!posted.ok) throw new Error(`re-post fixture 009: ${posted.status}`);
		await must('POST', `/api/v1/datasets/${DATASET}/runs`, { id: RUN_B, name: 'prompt v8' });
		const exported = await call('POST', '/v1/traces', exportOf(RUN_B, B_TRACES), 'application/x-protobuf');
		if (!exported.ok) throw new Error(`export run B: ${exported.status}`);
		const scores = [...A_TRACES, ...B_TRACES].map(([traceID, , value], i) => ({
			id: `5c0${String(i).padStart(29, '0')}`,
			trace_id: traceID,
			name: 'accuracy',
			value
		}));
		await must('POST', '/api/v1/scores', scores);
		for (const run of [RUN_A, RUN_B]) {
			await must('POST', `/api/v1/runs/${run}/finish`, {}, [409]);
		}
	})();
	return seeded;
}

test.beforeEach(async () => {
	await seed();
});

test('the section appears and navigates', async ({ page }) => {
	await signIn(page);
	const nav = page.getByRole('navigation', { name: 'Sections' });
	await expect(nav.getByText('Evals')).toBeVisible();
	await nav.getByRole('link', { name: 'Datasets' }).click();

	await expect(page).toHaveURL(/\/datasets$/);
	await expect(nav.getByRole('link', { name: 'Datasets' })).toHaveAttribute('aria-current', 'page');
	await page.getByRole('link', { name: DATASET }).click();
	await expect(page).toHaveURL(new RegExp(`/datasets/${DATASET}$`));
	await expect(page.getByRole('heading', { name: DATASET })).toBeVisible();
});

test('the dataset page lists items and switches versions', async ({ page }) => {
	await signIn(page);
	await page.goto(`/datasets/${DATASET}`);
	await expect(page.locator('tbody tr')).toHaveCount(2);
	await expect(page.locator('tbody tr').first()).toContainText('priority routing');

	// An older version is read-only and says so (spec 016 #4); version 0 is
	// the dataset before its first item.
	await page.getByLabel('Dataset version').fill('0');
	await page.getByLabel('Dataset version').press('Enter');
	await expect(page).toHaveURL(/version=0/);
	await expect(page.getByText(/Reading version 0 of/)).toBeVisible();
	await expect(page.getByText('No items at version 0')).toBeVisible();

	// And the runs tab is the other listing of the same thing.
	await page.getByRole('link', { name: 'Runs', exact: true }).last().click();
	await expect(page).toHaveURL(/tab=runs/);
	await expect(page.locator('tbody tr')).toHaveCount(2);
});

// A row's link is where a ⌘-click lands, and it has to carry the version in
// force: at the head the item is the same either way, but an item read at an
// older version and opened at the head is another item — or none.
test('an item row links to the version being read', async ({ page }) => {
	await signIn(page);
	await page.goto(`/datasets/${DATASET}?version=1`);

	const link = page.locator('tbody tr').first().getByRole('link');
	await expect(link).toHaveAttribute('href', /version=1/);
	await expect(link).toHaveAttribute('href', new RegExp(`peek=${ITEM_1}`));
});

test('an item opens in the panel, whole', async ({ page }) => {
	await signIn(page);
	await page.goto(`/datasets/${DATASET}`);
	await page.locator('tbody tr').first().getByRole('link').click();

	const panel = page.getByRole('dialog');
	await expect(panel).toBeVisible();
	await expect(page).toHaveURL(new RegExp(`peek=${ITEM_1}`));
	await expect(panel.getByLabel('Input', { exact: true })).toContainText('priority routing');
	await expect(panel.getByLabel('Expected output', { exact: true })).toContainText('Team plan');
	await expect(panel.getByRole('heading', { name: 'Versions' })).toBeVisible();
});

test('the run page shows the summary the API returns and the item peek drills into the trace and back', async ({
	page
}) => {
	await signIn(page);
	const summary = (await (await call('GET', `/api/v1/runs/${RUN_A}`)).json()) as {
		summary: { items: { covered: number; total: number }; traces: { count: number } };
	};
	await page.goto(`/runs/${RUN_A}`);

	await expect(page.getByRole('heading', { name: 'prompt v7' })).toBeVisible();
	await expect(page.getByText(`${summary.summary.items.covered} of ${summary.summary.items.total} covered`)).toBeVisible();
	await expect(page.getByRole('cell', { name: 'accuracy', exact: true })).toBeVisible();
	expect(summary.summary.traces.count).toBe(3);

	// The case, then its attempt's trace one level down, then back (#12).
	await page.getByRole('table', { name: 'Items' }).locator('tbody tr').first().getByRole('link').click();
	const panel = page.getByRole('dialog');
	await expect(panel.getByText('2 attempts')).toBeVisible();
	await panel.getByRole('button', { name: A_TRACES[0][0] }).click();
	await expect(page).toHaveURL(/trace=/);
	await expect(panel.getByRole('treeitem').first()).toBeVisible();
	await panel.getByRole('button', { name: 'Item' }).click();
	await expect(page).not.toHaveURL(/trace=/);
	await expect(panel.getByText('2 attempts')).toBeVisible();
});

test('the compare page renders header and verdicts, the toggle hides same, swap flips the URL', async ({
	page
}) => {
	await signIn(page);
	await page.goto(`/runs/${RUN_A}/compare/${RUN_B}`);

	// The header block is the response's: one score name, and the counts.
	await expect(page.getByText('1 improved · 0 regressed · 1 same')).toBeVisible();
	const rows = page.getByRole('table', { name: 'Cases' }).locator('tbody tr');
	await expect(rows).toHaveCount(2);
	await expect(page.getByText('improved', { exact: true })).toBeVisible();
	await expect(page.getByText('same', { exact: true })).toBeVisible();

	// The toggle filters the page, not the query, and the header stays.
	await page.getByRole('button', { name: 'Changed only' }).click();
	await expect(page).toHaveURL(/changed=1/);
	await expect(rows).toHaveCount(1);
	await expect(page.getByText('1 improved · 0 regressed · 1 same')).toBeVisible();

	// The panel walks what the table draws: with the toggle on, the hidden
	// `same` case is not a row `j`/`k` can reach.
	await rows.first().getByRole('link').click();
	const panel = page.getByRole('dialog');
	await expect(panel).toBeVisible();
	await expect(panel.getByRole('button', { name: 'Next row' })).toBeDisabled();
	await expect(panel.getByRole('button', { name: 'Previous row' })).toBeDisabled();
	await page.keyboard.press('Escape');

	await page.getByRole('link', { name: 'Swap' }).click();
	await expect(page).toHaveURL(new RegExp(`/runs/${RUN_B}/compare/${RUN_A}`));
});

test('two ticked runs of one dataset make Compare a link', async ({ page }) => {
	await signIn(page);
	await page.goto('/runs');
	await expect(page.getByRole('columnheader', { name: 'Dataset' })).toBeVisible();
	await expect(page.getByRole('link', { name: 'Compare' })).toHaveCount(0);

	const boxes = page.getByRole('checkbox');
	await boxes.nth(0).check();
	await boxes.nth(1).check();
	const compare = page.getByRole('link', { name: 'Compare' });
	await expect(compare).toHaveAttribute('href', /\/runs\/[0-9a-f]{32}\/compare\/[0-9a-f]{32}/);
});

test('score configs are listed read-only', async ({ page }) => {
	await signIn(page);
	await page.goto('/score-configs');
	const row = page.locator('tbody tr').filter({ hasText: 'accuracy' });
	await expect(row).toContainText('numeric');
	await expect(row).toContainText('higher');
	await expect(row).toContainText('0 … 1');
});

test('no screen scrolls the page sideways', async ({ page }) => {
	await signIn(page);
	for (const path of [
		'/datasets',
		`/datasets/${DATASET}`,
		`/datasets/${DATASET}?tab=runs`,
		'/runs',
		`/runs/${RUN_A}`,
		`/runs/${RUN_A}/compare/${RUN_B}`,
		'/score-configs'
	]) {
		await page.goto(path);
		await expect(page.getByRole('heading').first()).toBeVisible();
		const overflow = await page.evaluate(() => {
			const root = document.scrollingElement ?? document.documentElement;
			return root.scrollWidth - root.clientWidth;
		});
		expect(overflow, path).toBeLessThanOrEqual(0);
	}
});
