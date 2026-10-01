import { expect, type Page } from '@playwright/test';
import { test } from './fixtures';
import {
	signIn as enter,
	state,
	WIRE_GENERATION,
	WIRE_GUARDRAIL,
	WIRE_TRACE
} from './harness';

// What the wire already carries, end to end against the real binary
// (spec 012, Testing): the type filter, the TTFT column, the icons in the
// tree, and the prompt badge that leads to the traces that ran it.

async function signIn(page: Page) {
	await enter(page, state().member);
}

test('the type filter narrows the listing and survives a reload', async ({ page }) => {
	await signIn(page);
	await page.goto('/traces');

	await page.getByRole('button', { name: /Filters/ }).click();
	await page.getByLabel('Type').selectOption('tool');
	await page.getByRole('button', { name: 'Apply' }).click();

	// In the URL like every filter, so what somebody is looking at is a link.
	await expect(page).toHaveURL(/[?&]type=tool/);
	await expect(page.getByRole('link', { name: /\d/ }).first()).toBeVisible();
	// Two of the corpus's traces contain a tool call, and the rest do not.
	await expect(page.locator('tbody')).toHaveCount(2);
	await expect(page.locator('tbody')).toContainText(['checkout', 'research-agent']);

	await page.reload();
	await expect(page.locator('tbody')).toHaveCount(2);
	// The chip says what is narrowing the listing rather than hiding it
	// behind the button.
	await expect(page.getByRole('button', { name: /Remove filter Type: tool/ })).toBeVisible();
});

test('the listing has a TTFT column beside latency', async ({ page }, info) => {
	await signIn(page);
	await page.goto('/traces?type=tool');

	// The trace's TTFT is the *earliest* completion start among its
	// observations minus its own start — 180 ms here, from the tool call —
	// not the generation's own 388 ms, which the panel shows instead.
	if (info.project.name === 'mobile') {
		// A phone has no column for it: it folds under the name beside the
		// latency, and says which number it is (spec 006 #18). On the screen,
		// whole — a value cut to an ellipsis is still in the text content.
		await expect(page.getByRole('columnheader', { name: 'TTFT' })).toHaveCount(0);
		await expect(page.locator('tbody').first().getByText(/TTFT 180 ms/)).toBeInViewport({
			ratio: 1
		});
		return;
	}
	await expect(page.getByRole('columnheader', { name: 'TTFT' })).toBeVisible();
	await expect(page.locator('tbody').first()).toContainText('180 ms');
});

test('the tree draws the kind of every step', async ({ page }) => {
	await signIn(page);
	// No `obs=`: the panel opens on the tree, which is the pane a phone shows
	// first and the one the icons live in.
	await page.goto(`/traces?peek=${WIRE_TRACE}`);

	const tree = page.getByRole('tree');
	await expect(tree).toBeVisible();
	// An icon per kind, each carrying its name for a reader and for a screen
	// reader (spec 012, Application contract).
	for (const kind of ['span', 'generation', 'tool', 'guardrail']) {
		const icon = tree.getByLabel(kind).first();
		await expect(icon).toBeVisible();
		// The name is a `title` attribute as well as an `aria-label`, because
		// that is what draws the tooltip: ten glyphs and no visible label
		// leave a mouse reader nothing to hover. Asserted on the attribute
		// rather than on a `<title>` element, which is what the first attempt
		// at this did — a CSS type selector ignores namespaces, so it matched
		// the HTML `<title>` Svelte had put inside the `<svg>` and passed
		// while no tooltip existed (found in review of PR #19).
		await expect(icon).toHaveAttribute('title', kind);
		await expect(tree.getByTitle(kind).first()).toBeVisible();
	}
});

test('the panel shows the wait, the sizes and the prompt, and the badge leads to the listing', async ({
	page
}) => {
	await signIn(page);
	await page.goto(`/traces?type=tool&peek=${WIRE_TRACE}&obs=${WIRE_GENERATION}`);

	const panel = page.getByRole('dialog');
	await expect(panel).toBeVisible();

	// The timings block carries the wait before the first token.
	await expect(panel.getByText('TTFT')).toBeVisible();
	await expect(panel).toContainText('388 ms');

	// The badge names the prompt this generation ran.
	const badge = panel.getByRole('link', { name: /support-answer/ });
	await expect(badge).toBeVisible();
	await expect(badge).toContainText('v7');

	// And it leads to the traces that ran it.
	await badge.click();
	await expect(page).toHaveURL(/[?&]prompt=support-answer%407/);
	await expect(page.getByRole('button', { name: /Remove filter Prompt: support-answer@7/ })).toBeVisible();
	await expect(page.locator('tbody')).toHaveCount(1);
});

// A prompt name out of somebody else's namespace, with an `@` inside it and
// no version to tell that `@` from a separator. The badge is built from
// whatever the client sent, so a filter grammar that cannot read the name back
// makes the interface link at its own error (spec 012 #15, found in review of
// PR #19).
test('a prompt name with an @ inside it is a name, and its badge leads to the traces', async ({
	page
}) => {
	await signIn(page);
	await page.goto(`/traces?peek=${WIRE_TRACE}&obs=${WIRE_GUARDRAIL}`);

	const badge = page.getByRole('dialog').getByRole('link', { name: /team@acme\/answer/ });
	await expect(badge).toBeVisible();

	await badge.click();
	await expect(page).toHaveURL(/[?&]prompt=team%40acme%2Fanswer/);
	await expect(
		page.getByRole('button', { name: /Remove filter Prompt: team@acme\/answer/ })
	).toBeVisible();
	// The listing answers with the trace, rather than with the 400 the old
	// grammar produced.
	await expect(page.locator('tbody')).toHaveCount(1);
});

test('the statistics group by release, with the unnamed traces in their own row', async ({
	page
}) => {
	await signIn(page);
	// The window is named, as dashboard.spec.ts names it: the fixtures carry
	// fixed timestamps, and the default preset is the last seven days, which
	// they fell out of a week after this test was written.
	await page.goto('/dashboard?from=2026-08-01T00:00:00Z&to=2026-09-30T00:00:00Z');

	const releases = page.locator('section', { hasText: 'By release' });
	await expect(releases).toBeVisible();
	await expect(releases).toContainText('2026.8.30-rc1');
	// A trace that named no release is a bucket, not an omission.
	await expect(releases).toContainText('(no release)');
});
