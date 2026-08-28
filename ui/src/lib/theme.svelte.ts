// Both themes ship from day one (spec 006 #4). The default follows the
// system; a manual choice overrides it and is remembered.
//
// All the theme does is set `color-scheme` on the document, through a
// `data-theme` attribute. Every colour in `app.css` is a `light-dark()` pair
// that reads it, so switching costs one attribute and no re-render.

const STORAGE_KEY = 'tracepad.theme';

export type Theme = 'system' | 'light' | 'dark';

const THEMES: Theme[] = ['system', 'light', 'dark'];

class ThemeSetting {
	#value = $state.raw<Theme>('system');

	get value() {
		return this.#value;
	}

	/** Reads the remembered choice and applies it before the first paint. */
	restore() {
		let stored: string | null = null;
		try {
			stored = window.localStorage.getItem(STORAGE_KEY);
		} catch {
			// Storage is off; the system preference is a fine default.
		}
		this.set(isTheme(stored) ? stored : 'system');
	}

	set(theme: Theme) {
		this.#value = theme;
		const root = document.documentElement;
		if (theme === 'system') delete root.dataset.theme;
		else root.dataset.theme = theme;
		try {
			if (theme === 'system') window.localStorage.removeItem(STORAGE_KEY);
			else window.localStorage.setItem(STORAGE_KEY, theme);
		} catch {
			// The choice lasts this tab; nothing else breaks.
		}
	}

	/** The toggle's behaviour: system → light → dark → system. */
	cycle() {
		this.set(THEMES[(THEMES.indexOf(this.#value) + 1) % THEMES.length]);
	}
}

function isTheme(value: string | null): value is Theme {
	return value !== null && (THEMES as string[]).includes(value);
}

export const theme = new ThemeSetting();
