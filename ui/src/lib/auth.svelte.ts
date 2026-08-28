import { goto, replaceState } from '$app/navigation';

// The app always authenticates (spec 006 #8). There is no localhost bypass:
// "is this localhost?" is answered from the connection's remote address, and
// behind any local reverse proxy every request looks like loopback — which
// would turn the check off for the internet on the standard TLS setup.
//
// The credential is a project secret key or the admin token, kept in
// localStorage and sent as `Authorization: Bearer`. The zero-friction first
// contact is the pre-authed URL the server prints on first run: the key rides
// in the fragment, which is never sent in a request, and the app stores it and
// strips it out of the address bar before anything else happens.
//
// Module-level state rather than context: the app is a client-only SPA
// (ssr = false), so there is no server request to leak it between, and the key
// is genuinely global — the API client needs it from outside any component
// tree.

const STORAGE_KEY = 'tracepad.key';

/** Where an unauthenticated visit is sent, and what it comes back to. */
export const LOGIN_ROUTE = '/login';

class Auth {
	#key = $state.raw<string | null>(null);

	/** The stored credential, or null when there is none. */
	get key() {
		return this.#key;
	}

	get authenticated() {
		return this.#key !== null;
	}

	/**
	 * Reads the credential the app starts with: whatever a pre-authed URL
	 * carries, otherwise whatever the last visit stored.
	 *
	 * This runs before the router exists, so it only reads the fragment;
	 * taking it back out of the address bar is `stripFragment`, which the
	 * shell calls once it is mounted.
	 */
	restore() {
		const hash = window.location.hash;
		const fromLink = hash.startsWith('#')
			? new URLSearchParams(hash.slice(1)).get('key')
			: null;
		if (fromLink && this.adopt(fromLink)) return;
		this.#key = read(STORAGE_KEY);
	}

	/**
	 * Takes the key back out of the URL. `replaceState` rather than a push:
	 * the pre-authed link must not survive in history, where a back button or
	 * a shared screen would hand the secret to whoever is looking.
	 */
	stripFragment() {
		if (!window.location.hash) return;
		replaceState(window.location.pathname + window.location.search, {});
	}

	/** Stores a credential entered by hand at the login screen. */
	adopt(key: string): boolean {
		const trimmed = key.trim();
		if (!trimmed) return false;
		this.#key = trimmed;
		write(STORAGE_KEY, trimmed);
		return true;
	}

	/** Forgets the credential without navigating; the sign-out path. */
	clear() {
		this.#key = null;
		write(STORAGE_KEY, null);
	}

	/**
	 * What a 401 means: the stored key is wrong, revoked, or belongs to a
	 * deleted project. Keeping it would retry the same failure on every
	 * screen, so it is dropped and the person is asked for another one.
	 */
	reject() {
		if (!this.#key) return;
		this.clear();
		goto(LOGIN_ROUTE, { replaceState: true });
	}

}

// Storage is a browser feature a person can switch off, and a private window
// throws on access rather than returning nothing. Neither is a reason for the
// app to fail to load: without it the key simply lasts one session.
function read(name: string): string | null {
	try {
		return window.localStorage.getItem(name);
	} catch {
		return null;
	}
}

function write(name: string, value: string | null) {
	try {
		if (value === null) window.localStorage.removeItem(name);
		else window.localStorage.setItem(name, value);
	} catch {
		// Nothing to do and nothing worth saying: the app works, the key
		// just will not outlive the tab.
	}
}

/**
 * Where the login form sends somebody after they sign in: back to the screen
 * the guard interrupted, and never anywhere else.
 *
 * The check is an origin comparison rather than a prefix test, because the URL
 * parser is the only thing that agrees with the URL parser: for a special
 * scheme it treats `/\` exactly like `//`, so `?next=/\elsewhere.example`
 * parses to a different origin while passing any "starts with one slash"
 * rule. Nothing is redirected off-site either way — SvelteKit refuses a
 * cross-origin `goto` — but that refusal would land as a failed sign-in on a
 * reader who is in fact signed in.
 */
export function returnTo(url: URL, fallback = '/traces'): string {
	const asked = url.searchParams.get('next');
	if (!asked) return fallback;
	try {
		const target = new URL(asked, url.origin);
		return target.origin === url.origin ? target.pathname + target.search : fallback;
	} catch {
		return fallback;
	}
}

export const auth = new Auth();
