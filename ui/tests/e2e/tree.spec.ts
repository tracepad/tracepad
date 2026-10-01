import { expect } from '@playwright/test';
import { test } from './fixtures';
import { createProject, signIn, state } from './harness';

// A trace larger than one tree (spec 043 #18): the server answers its first
// 10,000 observations by start time and counts the rest, and the trace view
// says so above the tree, so a reader does not take the part for the whole.
//
// Its own project, so that the other suites' counts do not include a trace of
// ten thousand spans.

const TRACE = 'ee000000000000000000000000000001';
const SPANS = 10_001;

let own: ReturnType<typeof createProject> | null = null;
const project = () => (own ??= createProject('tree'));

let seeded: Promise<void> | null = null;

/** One trace of SPANS flat spans a millisecond apart, in one export. */
function seed(): Promise<void> {
	seeded ??= (async () => {
		const { baseURL } = state();
		const { key } = await project();
		const base = Date.parse('2026-09-20T10:00:00Z') * 1_000_000;
		const spans = Array.from({ length: SPANS }, (_, n) => ({
			traceId: TRACE,
			spanId: (n + 1).toString(16).padStart(16, '0'),
			name: `step-${n + 1}`,
			startTimeUnixNano: String(base + n * 1_000_000),
			endTimeUnixNano: String(base + n * 1_000_000 + 500_000)
		}));
		const response = await fetch(`${baseURL}/v1/traces`, {
			method: 'POST',
			headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
			body: JSON.stringify({
				resourceSpans: [{ resource: { attributes: [] }, scopeSpans: [{ spans }] }]
			})
		});
		if (!response.ok) throw new Error(`seed: ${response.status} ${await response.text()}`);
	})();
	return seeded;
}

test('a trace past the tree ceiling says how many observations are not shown', async ({ page }) => {
	test.setTimeout(60_000);
	await seed();
	await signIn(page, (await project()).account);
	await page.goto(`/traces/${TRACE}`);

	const notice = page.getByRole('note');
	await expect(notice).toHaveText(
		'Showing the first 10,000 of 10,001 observations by start time; 1 is not shown.',
		{ timeout: 30_000 }
	);
	// The first span to start is in the tree.
	await expect(page.getByText('step-1', { exact: true }).first()).toBeVisible();
});
