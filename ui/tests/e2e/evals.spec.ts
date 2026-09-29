import { expect, test, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { clipped, createProject, foldsAt, openDialog, section, sideways, signIn as enter, state } from './harness';

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
let own: ReturnType<typeof createProject> | null = null;
const project = () => (own ??= createProject('evals'));

/** Signs in with the project's key by hand, the way a second project is opened. */
async function signIn(page: Page) {
	await enter(page, (await project()).account);
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
	const datasets = await section(page, 'Datasets');
	// The group's heading, in whichever list holds the link: the column, or
	// on a phone the *More* sheet.
	const menu = datasets.locator('xpath=ancestor::nav[1]');
	await expect(menu.getByText('Evals', { exact: true })).toBeVisible();
	await datasets.click();

	await expect(page).toHaveURL(/\/datasets$/);
	await expect(await section(page, 'Datasets')).toHaveAttribute('aria-current', 'page');
	await page.keyboard.press('Escape');
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
}, testInfo) => {
	await signIn(page);
	const summary = (await (await call('GET', `/api/v1/runs/${RUN_A}`)).json()) as {
		summary: { items: { covered: number; total: number }; traces: { count: number } };
	};
	await page.goto(`/runs/${RUN_A}`);

	await expect(page.getByRole('heading', { name: 'prompt v7' })).toBeVisible();
	await expect(page.getByText(`${summary.summary.items.covered} of ${summary.summary.items.total} covered`)).toBeVisible();
	// The score's own cell on a desktop; on a phone the cell that holds its
	// name first, then its type, count and mean (spec 006 #24).
	await expect(
		page.getByRole('cell', testInfo.project.name === 'mobile' ? { name: /^accuracy numeric/ } : { name: 'accuracy', exact: true })
	).toBeVisible();
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

	// The header block is the response's: one score name, and the counts —
	// a column on a desktop, folded under the name on a phone (spec 006 #24).
	const scores = page.locator('main table').filter({ hasText: 'Delta' });
	await expect(scores).toContainText('1 improved · 0 regressed · 1 same');
	const rows = page.getByRole('table', { name: 'Cases' }).locator('tbody tr');
	await expect(rows).toHaveCount(2);
	await expect(page.getByText('improved', { exact: true })).toBeVisible();
	await expect(page.getByText('same', { exact: true })).toBeVisible();

	// The toggle filters the page, not the query, and the header stays.
	await page.getByRole('button', { name: 'Changed only' }).click();
	await expect(page).toHaveURL(/changed=1/);
	await expect(rows).toHaveCount(1);
	await expect(scores).toContainText('1 improved · 0 regressed · 1 same');

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

test('two ticked runs of one dataset make Compare a link', async ({ page }, testInfo) => {
	await signIn(page);
	await page.goto('/runs');
	// The dataset is a column on a desktop and a link under the name on a
	// phone (spec 006 #22): a link either way.
	if (testInfo.project.name !== 'mobile') {
		await expect(page.getByRole('columnheader', { name: 'Dataset' })).toBeVisible();
	}
	await expect(page.getByRole('link', { name: DATASET }).first()).toBeVisible();
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

// --- the write half (spec 016, PR 2) -----------------------------------------
//
// Every one of these tests owns what it writes to. The two Playwright projects
// run this file against one server, and the tests of the read half assert
// exact numbers of the corpus above — so a dataset created here, deleted here
// and named after nothing else is what keeps a write from moving a count
// somebody is reading.

/** A dataset of this test's own, with one case in it, at version 1. */
async function freshDataset(prefix: string): Promise<string> {
	const name = `${prefix}-${Math.random().toString(36).slice(2, 8)}`;
	await must('POST', `/api/v1/datasets/${name}/items`, {
		input: { question: 'what is the refund window?' },
		expected_output: { answer: '30 days.' }
	});
	return name;
}

/** Replaces a pane's document, the way an author would after selecting all. */
async function type(page: Page, label: string, text: string) {
	const pane = page.getByLabel(label, { exact: true });
	await pane.click();
	await page.keyboard.press('ControlOrMeta+a');
	await page.keyboard.type(text);
}

test('an item is edited into a new version, and saving it again changes nothing', async ({
	page
}) => {
	const name = await freshDataset('edit');
	await signIn(page);
	await page.goto(`/datasets/${name}`);

	// In through the peek panel, which is where a reader finds the case.
	await page.locator('tbody tr').first().getByRole('link').click();
	await page.getByRole('dialog').getByRole('link', { name: 'Edit', exact: true }).click();
	await expect(page).toHaveURL(/\/items\/[0-9a-f]{32}\/edit$/);
	await expect(page.getByLabel('Input', { exact: true })).toContainText('refund window');

	await type(page, 'Expected output', '{"answer": "30 days from delivery."}');
	await page.getByRole('button', { name: 'Save' }).click();
	await expect(page.getByText('Saved as version 2.')).toBeVisible();

	// The same body again: the store writes nothing and says so (spec 014 #6).
	await page.getByRole('button', { name: 'Save' }).click();
	await expect(page.getByText(/^Unchanged/)).toContainText('version 2');

	// And the new version is what the dataset now reads at.
	await page.goto(`/datasets/${name}`);
	await expect(page.locator('tbody tr').first()).toContainText('30 days from delivery');
});

test('a case is cut from an observation into a dataset, whole', async ({ page }) => {
	const name = await freshDataset('cut');
	await signIn(page);
	await page.goto(`/traces/${A_TRACES[0][0]}`);

	// The generation, not the root: in an agent trace the case is usually one
	// generation (#8).
	await page.getByRole('treeitem', { name: /^generation answer/ }).click();
	await page.getByRole('link', { name: 'Add to dataset' }).click();

	await expect(page).toHaveURL(/\/datasets\/items\/new\?/);
	// The payloads are the observation's own, fetched whole rather than taken
	// from a preview: the input becomes the case, the output what it should
	// have said.
	await expect(page.getByLabel('Input', { exact: true })).toContainText('priority routing');
	await expect(page.getByLabel('Expected output', { exact: true })).toContainText('Team plan');

	// The correction first, the dataset second — which is the order the gesture
	// is actually used in, and the order that catches a page re-seeding its
	// panes when `?dataset=` changes under it (found in review of this PR).
	await type(page, 'Expected output', '{"answer": "The Team plan and above, plus Enterprise."}');
	await page.getByLabel('Dataset').selectOption(name);
	await expect(page.getByLabel('Expected output', { exact: true })).toContainText('Enterprise');

	await page.getByRole('button', { name: 'Save' }).click();
	await expect(page.getByText('Saved as version 2.')).toBeVisible();

	// The case that was stored is the corrected one, and it knows where it was
	// cut from.
	await page.getByRole('link', { name: 'Open it in the dataset' }).click();
	const panel = page.getByRole('dialog');
	await expect(panel.getByText('Cut from')).toBeVisible();
	await expect(panel.getByLabel('Expected output', { exact: true })).toContainText('Enterprise');
});

// A `?dataset=` that names nothing is a stale link or a typo, and the items
// endpoint would bring that name into being on the first write (spec 014).
test('the editor refuses to save into a dataset the project does not have', async ({ page }) => {
	const name = await freshDataset('typo');
	await signIn(page);
	await page.goto(`/datasets/items/new?dataset=${name}x`);

	await expect(page.getByText(/no dataset called/)).toBeVisible();
	await type(page, 'Input', '{"question": "anything"}');
	// The linter settles about 300 ms after the last keystroke (spec 015 #15),
	// so the wait is what makes "still disabled" mean anything; the control
	// below is what proves the wait is long enough.
	await page.waitForTimeout(1000);
	const save = page.getByRole('button', { name: 'Save' });
	await expect(save).toBeDisabled();

	// Choosing a real one opens it — and keeps what was typed, which is the
	// same promise the `?dataset=` rewrite makes above.
	await page.getByLabel('Dataset').selectOption(name);
	await expect(save).toBeEnabled();
});

test('deleting a dataset shows the dry run and refuses a wrong echo', async ({ page }) => {
	const name = await freshDataset('doomed');
	await signIn(page);
	await page.goto(`/datasets/${name}`);
	await page.getByRole('button', { name: 'Delete dataset' }).click();

	const card = page.getByRole('dialog');
	await card.getByRole('button', { name: 'Show what would go' }).click();
	// The server's own counts, not the screen's (spec 007 #5).
	await expect(card.getByText('This would delete')).toBeVisible();
	// Including what is *not* deleted: the traces the runs were pinning.
	await expect(card.getByText(/pinned traces/)).toBeVisible();

	const execute = card.getByRole('button', { name: 'Delete this dataset' });
	await expect(execute).toBeDisabled();
	await card.getByRole('textbox').fill(`${name}x`);
	await expect(execute).toBeDisabled();

	await card.getByRole('textbox').fill(name);
	await expect(execute).toBeEnabled();
	await execute.click();
	await expect(page).toHaveURL(/\/datasets$/);
	await expect(page.getByRole('link', { name })).toHaveCount(0);
});

test('a score config is written, edited and removed through the form', async ({ page }) => {
	const name = `helpfulness-${Math.random().toString(36).slice(2, 8)}`;
	await signIn(page);
	await page.goto('/score-configs');

	const form = await openDialog(page.getByRole('button', { name: 'New score config' }));
	await form.getByLabel('Name').fill(name);
	await form.getByLabel('Type').selectOption('categorical');
	// A categorical name has no direction and must have its categories: the
	// form says so before the round trip does (spec 014 #16).
	await expect(form.getByLabel('Direction')).toHaveCount(0);
	await expect(form.getByRole('button', { name: 'Save' })).toBeDisabled();
	await form.getByLabel('Categories, one per line').fill('helpful\nunhelpful');
	await form.getByRole('button', { name: 'Save' }).click();

	const row = page.locator('tbody tr').filter({ hasText: name });
	await expect(row).toContainText('categorical');
	await expect(row).toContainText('helpful, unhelpful');

	// The same form edits it, and the name is not a thing an edit changes.
	await openDialog(row.getByRole('button', { name: 'Edit' }));
	await expect(form.getByLabel('Name')).toHaveAttribute('readonly', '');
	// The focus starts past the read-only name (spec 006 #26).
	await expect(form.getByLabel('Type')).toBeFocused();
	await form.getByLabel('Description').fill('Whether the answer helped');
	await form.getByRole('button', { name: 'Save' }).click();
	await expect(row).toContainText('Whether the answer helped');

	await row.getByRole('button', { name: 'Remove' }).click();
	await page.getByRole('alertdialog').getByRole('button', { name: 'Remove the config' }).click();
	await expect(page.locator('tbody tr').filter({ hasText: name })).toHaveCount(0);
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
		'/score-configs',
		`/datasets/items/new?dataset=${DATASET}`,
		`/datasets/${DATASET}/items/${ITEM_1}/edit`
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

// Spec 006 #22, #24: on a phone every eval table folds to the width it is
// given — the columns that name a row stay, the rest go under it — and none
// of them scrolls sideways in its own box either.
test('on a phone the eval listings fold rather than scroll', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name !== 'mobile', 'the narrow width is the test');
	await signIn(page);
	for (const [path, table, heads] of [
		['/datasets', page.locator('main table').first(), ['Name']],
		[`/datasets/${DATASET}`, page.locator('main table').first(), ['Id', 'Input']],
		['/runs', page.locator('main table').first(), ['Compare', 'Name', 'Status']],
		[`/runs/${RUN_A}`, page.getByRole('table', { name: 'Items' }), ['Item', 'Scores']],
		[`/runs/${RUN_A}`, page.locator('main table').filter({ hasText: 'Range or distribution' }), ['Name', 'Range or distribution']],
		[`/runs/${RUN_A}/compare/${RUN_B}`, page.locator('main table').filter({ hasText: 'Delta' }), ['Name', 'Delta']],
		[`/runs/${RUN_A}/compare/${RUN_B}`, page.getByRole('table', { name: 'Cases' }), ['Item', 'Scores']],
		['/score-configs', page.locator('main table').first(), ['Name', 'Actions']]
	] as const) {
		await page.goto(path);
		await expect(table.locator('tbody tr').first()).toBeVisible();
		await expect(table.locator('thead th'), path).toHaveText([...heads]);
		expect(await sideways(table), path).toBeLessThanOrEqual(0);
		expect(await clipped(table), path).toEqual([]);
	}
});

// Spec 006 #22, #24: what a program named is bounded at its column, so a
// categorical label longer than the score's columns, a score name and a
// metadata key that is a sentence leave the tables inside their boxes, on a
// phone and on a desktop. The comparison's own numbers come from the demo data
// otherwise, and its labels are short.
test('long labels, score names and a long metadata key leave the comparison in its boxes', async ({
	page
}, testInfo) => {
	await signIn(page);
	const hex = () => Array.from({ length: 32 }, () => Math.floor(Math.random() * 16).toString(16)).join('');
	const dataset = `labels-${hex().slice(0, 6)}`;
	const item = hex();
	await must('POST', `/api/v1/datasets/${dataset}/items`, [{ id: item, input: { question: 'q' } }]);
	const key = 'a_metadata_key_that_is_a_whole_sentence_about_the_run_'.repeat(2);
	const runs = [hex(), hex()];
	const name = 'a_score_whose_name_is_longer_than_its_column';
	const scores: unknown[] = [];
	for (const [i, run] of runs.entries()) {
		await must('POST', `/api/v1/datasets/${dataset}/runs`, {
			id: run,
			name: `run ${i}`,
			metadata: { [key]: i ? 'a much longer value, '.repeat(12) : 'short', model: 'claude-sonnet-4-5-20250929' }
		});
		const traces = [
			[hex(), item, 1],
			[hex(), item, 1]
		] as const;
		const exported = await call('POST', '/v1/traces', exportOf(run, traces), 'application/x-protobuf');
		if (!exported.ok) throw new Error(`export run: ${exported.status}`);
		for (const [j, [trace]] of traces.entries()) {
			scores.push({
				id: hex(),
				trace_id: trace,
				name,
				data_type: 'categorical',
				string_value: j ? 'friendly_handoff_completed' : i ? 'needs_escalation_to_a_person' : 'friendly'
			});
		}
	}
	await must('POST', '/api/v1/scores', scores);

	await page.goto(`/runs/${runs[0]}/compare/${runs[1]}`);
	const table = page.locator('main table').filter({ hasText: 'Delta' });
	await expect(table).toContainText(name);
	const cases = page.getByRole('table', { name: 'Cases' });
	const metadata = page.locator('main section').filter({ hasText: 'Metadata that differs' }).locator('table');
	await expect(metadata).toContainText('short');
	if (testInfo.project.name === 'mobile') {
		expect(await sideways(table)).toBeLessThanOrEqual(0);
		expect(await sideways(cases)).toBeLessThanOrEqual(0);
		expect(await sideways(metadata)).toBeLessThanOrEqual(0);
		expect(await clipped(table)).toEqual([]);
		return;
	}
	await foldsAt(page, table, 608, 6, 34);
	await foldsAt(page, cases, 312 + 112, 4);
	for (const width of [820, 1440]) {
		await page.setViewportSize({ width, height: 900 });
		expect(await sideways(table), `${width}`).toBeLessThanOrEqual(0);
		expect(await sideways(cases), `${width}`).toBeLessThanOrEqual(0);
		expect(await sideways(metadata), `${width}`).toBeLessThanOrEqual(0);
	}
});

// Spec 006 #22: on a desktop each table has all its columns from its own
// width — the runs' 896 px, the runs of one dataset's 720, the datasets' 880
// and so on — and folds under it, however the window got that size.
test('the eval tables fold at their own widths on a desktop', async ({ page }, testInfo) => {
	test.skip(testInfo.project.name === 'mobile', 'a desktop window is the test');
	await signIn(page);
	// The last number is what a card puts around its table: two 16 px gutters
	// and a border on the run and comparison screens.
	for (const [path, table, box, columns, around] of [
		['/datasets', page.locator('main table').first(), 880, 6, 0],
		[`/datasets/${DATASET}`, page.locator('main table').first(), 672, 5, 0],
		[`/datasets/${DATASET}?tab=runs`, page.locator('main table').first(), 720, 6, 0],
		['/runs', page.locator('main table').first(), 896, 7, 0],
		[`/runs/${RUN_A}`, page.getByRole('table', { name: 'Items' }), 672, 4, 0],
		[`/runs/${RUN_A}`, page.locator('main table').filter({ hasText: 'Range or distribution' }), 496, 5, 34],
		[`/runs/${RUN_A}/compare/${RUN_B}`, page.locator('main table').filter({ hasText: 'Delta' }), 608, 6, 34],
		[`/runs/${RUN_A}/compare/${RUN_B}`, page.getByRole('table', { name: 'Cases' }), 424, 4, 0],
		['/score-configs', page.locator('main table').first(), 912, 6, 0]
	] as const) {
		await page.goto(path);
		await expect(table.locator('tbody tr').first(), path).toBeVisible();
		await foldsAt(page, table, box, columns, around);
	}
});
