import { defineConfig, devices } from '@playwright/test';
import { CI, PORT } from './tests/e2e/harness';

// The end-to-end smoke (spec 006 #10). It is the only suite that sees the
// seams the unit layer cannot: the bundle inside the binary, the SPA fallback,
// the pre-authed URL the server prints, and a payload fetched from the one
// budget-exempt endpoint. It boots the real binary on a temp database, so it
// is a separate CI job rather than part of the 30-second gate.

export default defineConfig({
	testDir: 'tests/e2e',
	globalSetup: './tests/e2e/global-setup.ts',
	fullyParallel: true,
	// Playwright's default is a worker per two cores, which on the two-core
	// runner is one — a suite that two projects (desktop and mobile) had
	// already doubled ran there on a single worker. The tests wait on a
	// browser and the server far more than they compute, so on CI a worker
	// per core is the right ratio, and a percentage rather than a number
	// keeps it right when the runner grows. A laptop keeps the default.
	workers: CI ? '100%' : undefined,
	forbidOnly: CI,
	retries: CI ? 1 : 0,
	reporter: CI ? 'github' : 'list',
	use: {
		baseURL: `http://127.0.0.1:${PORT}`,
		trace: 'retain-on-failure'
	},
	projects: [
		{ name: 'desktop', use: { ...devices['Desktop Chrome'] } },
		// A phone is a supported width, not an afterthought (spec 006 #15), so
		// the scenarios run again at 375 px portrait with a coarse pointer.
		{
			name: 'mobile',
			use: { ...devices['Pixel 5'], viewport: { width: 375, height: 812 } }
		}
	]
});
