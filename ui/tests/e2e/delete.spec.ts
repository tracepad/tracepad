import { readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { expect, type Page } from '@playwright/test';
import { test } from './fixtures';
import { createProject, inviteViewer, signIn as enter, state } from './harness';

// Deleting traces (spec 035, Testing — UI), against the real binary: one from
// the peek panel through the header's dialog, and a filter's worth from the
// listing's dialog in rounds with *Stop* in between — and neither offered to a
// viewer.
//
// Its own project (spec 016 #18): this suite deletes, which is not a thing to
// do to the project the other suites count. Serial, because each step is the
// state the next one starts from.

test.describe.configure({ mode: 'serial' });

/** Fixture 001's trace: the one deleted by hand. */
const TRACE = '4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f';
/** A synthetic corpus past one round, filtered by its own environment. */
const BULK = 1100;
const BULK_ENVIRONMENT = 'loadtest';

let own: ReturnType<typeof createProject> | null = null;
const project = () => (own ??= createProject('delete'));

async function signIn(page: Page) {
	await enter(page, (await project()).account);
}

async function post(path: string, body: Uint8Array | object, type: string) {
	const { baseURL } = state();
	const { key } = await project();
	const response = await fetch(`${baseURL}${path}`, {
		method: 'POST',
		headers: { Authorization: `Bearer ${key}`, 'Content-Type': type },
		body: body instanceof Uint8Array ? Buffer.from(body) : JSON.stringify(body)
	});
	if (!response.ok) throw new Error(`POST ${path}: ${response.status} ${await response.text()}`);
}

/**
 * How many traces the project holds under a filter, counted exactly: the
 * listing's count stops at a thousand, and the deletion's own dry run is the
 * one read that does not.
 */
async function held(query: string): Promise<number> {
	const { baseURL } = state();
	const { key } = await project();
	const response = await fetch(`${baseURL}/api/v1/traces?to=2030-01-01T00:00:00Z${query}`, {
		method: 'DELETE',
		headers: { Authorization: `Bearer ${key}` }
	});
	const body = (await response.json()) as { matched: number };
	return body.matched;
}

let seeded: Promise<void> | null = null;

/** Fixture 001, and a bulk corpus of single-span traces an hour apart in two hours. */
function seed(): Promise<void> {
	seeded ??= (async () => {
		const fixture = readFileSync(
			join(resolve(process.cwd(), '..'), 'testdata', 'otlp', '001-langfuse-sdk-generation.pb')
		);
		await post('/v1/traces', fixture, 'application/x-protobuf');

		const base = Date.parse('2026-09-10T10:00:00Z') * 1_000_000;
		const spans = Array.from({ length: BULK }, (_, n) => {
			const start = base + n * 1_000_000_000;
			return {
				traceId: `bb${(n + 1).toString(16).padStart(30, '0')}`,
				spanId: `bb${(n + 1).toString(16).padStart(14, '0')}`,
				name: 'load',
				startTimeUnixNano: String(start),
				endTimeUnixNano: String(start + 5_000_000),
				attributes: [
					{ key: 'langfuse.environment', value: { stringValue: BULK_ENVIRONMENT } }
				]
			};
		});
		await post(
			'/v1/traces',
			{ resourceSpans: [{ resource: { attributes: [] }, scopeSpans: [{ spans }] }] },
			'application/json'
		);
	})();
	return seeded;
}

test.beforeEach(async () => {
	await seed();
});

test('one trace goes from the peek panel, and its row with it', async ({ page }) => {
	await signIn(page);
	await page.goto(`/traces?peek=${TRACE}`);
	const panel = page.getByRole('dialog');
	await expect(panel.getByRole('treeitem').first()).toBeVisible();

	await panel.getByRole('button', { name: 'Delete…' }).click();
	const dialog = page.getByRole('dialog', { name: 'Delete this trace' });
	// The dry run is on screen as the dialog opens, the id already typed.
	await expect(dialog.getByText('This would delete')).toBeVisible();
	await expect(dialog.getByText(/raw OTLP bodies are not deleted/)).toBeVisible();
	await expect(dialog.getByRole('textbox')).toHaveValue(TRACE);
	await dialog.getByRole('button', { name: 'Delete this trace' }).click();

	// The panel closes, the listing re-reads, and the row is gone.
	await expect(page).not.toHaveURL(/peek=/);
	await expect(page.locator('tbody tr').filter({ hasText: 'support-chat' })).toHaveCount(0);
	expect(await held(`&environment=${BULK_ENVIRONMENT}`)).toBe(BULK);
});

test('a filter goes from the listing in rounds, and Stop leaves the rest', async ({ page }) => {
	await signIn(page);
	await page.goto(`/traces?environment=${BULK_ENVIRONMENT}`);
	await expect(page.getByText('1,000+').first()).toBeVisible();

	// The confirmed rounds are held for a moment on the wire so that Stop
	// can land while the first is in flight: the server does the real
	// deletion either way, and what is being tested is what the dialog does
	// with the answer.
	await page.route(
		(url) => url.pathname.endsWith('/api/v1/traces') && url.searchParams.has('confirm'),
		async (route) => {
			if (route.request().method() !== 'DELETE') return route.continue();
			await new Promise((resolve) => setTimeout(resolve, 800));
			await route.continue();
		}
	);

	await page.getByRole('button', { name: 'Delete…' }).click();
	const dialog = page.getByRole('dialog', { name: 'Delete the traces these filters match' });
	// The filter as the bar's chip, and the moment the set was closed at.
	await expect(dialog.getByText(`Environment: ${BULK_ENVIRONMENT}`)).toBeVisible();
	await expect(dialog.getByText(/the moment this dialog opened/)).toBeVisible();
	// The exact count, past the listing's cap.
	await expect(dialog.getByText('1,100').first()).toBeVisible();

	await dialog.getByRole('textbox').fill((await project()).name);
	await dialog.getByRole('button', { name: 'Delete these traces' }).click();
	await dialog.getByRole('button', { name: 'Stop' }).click();
	await expect(dialog.getByRole('button', { name: 'Stopping after this round' })).toBeDisabled();
	await expect(dialog.getByText(/Stopped after 1,000 traces/)).toBeVisible({ timeout: 15_000 });
	await expect(dialog.getByRole('status', { name: 'Progress' })).toHaveText(/1,000 of 1,100 deleted/);
	expect(await held(`&environment=${BULK_ENVIRONMENT}`)).toBe(BULK - 1000);

	// The rest, to the end: the same dialog, a fresh dry run.
	await page.keyboard.press('Escape');
	await page.unrouteAll();
	await page.getByRole('button', { name: 'Delete…' }).click();
	await expect(dialog.getByText('100', { exact: true }).first()).toBeVisible();
	await dialog.getByRole('textbox').fill((await project()).name);
	await dialog.getByRole('button', { name: 'Delete these traces' }).click();
	await expect(dialog.getByText('Deleted 100 traces.')).toBeVisible({ timeout: 15_000 });
	await expect(dialog.getByRole('status', { name: 'Progress' })).toHaveText(/100 of 100 deleted/);
	await page.keyboard.press('Escape');

	// The listing re-read: nothing matches the filter any more.
	await expect(page.getByText('No trace matches these filters')).toBeVisible();
	expect(await held(`&environment=${BULK_ENVIRONMENT}`)).toBe(0);
});

test('a viewer is offered neither', async ({ page }) => {
	const { baseURL } = state();
	const { id, name } = await project();
	await post('/v1/traces', readFileSync(
		join(resolve(process.cwd(), '..'), 'testdata', 'otlp', '008-wire-columns.pb')
	), 'application/x-protobuf');
	await enter(page, await inviteViewer(baseURL, id, name));

	await page.goto('/traces');
	await expect(page.locator('tbody tr').first()).toBeVisible();
	await expect(page.getByRole('button', { name: 'Delete…' })).toHaveCount(0);
	await page.locator('tbody tr').first().getByRole('link').first().click();
	await expect(page.getByRole('dialog').getByRole('treeitem').first()).toBeVisible();
	await expect(page.getByRole('button', { name: 'Delete…' })).toHaveCount(0);
});
