import { beforeEach, describe, expect, it, vi } from 'vitest';

async function freshTheme() {
	vi.resetModules();
	return (await import('./theme.svelte')).theme;
}

beforeEach(() => {
	window.localStorage.clear();
	delete document.documentElement.dataset.theme;
});

describe('the theme setting', () => {
	it('follows the system until someone says otherwise', async () => {
		const theme = await freshTheme();

		theme.restore();

		expect(theme.value).toBe('system');
		// No attribute means `color-scheme: light dark`, which is what makes
		// every `light-dark()` token follow the operating system.
		expect(document.documentElement.dataset.theme).toBeUndefined();
	});

	it('pins the scheme when a choice is made, and remembers it', async () => {
		const theme = await freshTheme();
		theme.restore();

		theme.set('dark');

		expect(document.documentElement.dataset.theme).toBe('dark');
		expect(window.localStorage.getItem('tracepad.theme')).toBe('dark');

		const later = await freshTheme();
		later.restore();
		expect(later.value).toBe('dark');
		expect(document.documentElement.dataset.theme).toBe('dark');
	});

	it('cycles system → light → dark → system', async () => {
		const theme = await freshTheme();
		theme.restore();

		theme.cycle();
		expect(theme.value).toBe('light');
		theme.cycle();
		expect(theme.value).toBe('dark');
		theme.cycle();
		expect(theme.value).toBe('system');
		expect(window.localStorage.getItem('tracepad.theme')).toBeNull();
	});

	it('ignores a stored value it does not recognise', async () => {
		window.localStorage.setItem('tracepad.theme', 'solarized');
		const theme = await freshTheme();

		theme.restore();

		expect(theme.value).toBe('system');
	});
});
