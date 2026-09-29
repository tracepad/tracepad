import { goto } from '$app/navigation';
import type { Account, Me, Membership } from '$lib/api/client.svelte';
import { accountName } from '$lib/format';

// Who is signed in (spec 028 #13). There is no credential in this file any
// more: the session is an `HttpOnly` cookie the browser holds and no script can
// read, so "are we signed in" is not a value we keep but a question the server
// answers — `GET /api/v1/auth/me`, once on load, and again whenever a rename or
// a role change makes the answer stale.
//
// What is kept here is that answer: the account, and the projects it can reach
// with the role it has in each. Every screen reads its role from this list
// rather than asking per screen (Decision 15), and the shell reads the name.
//
// Module-level state rather than context: the app is a client-only SPA
// (ssr = false), so there is no server request to leak it between, and the API
// client needs it from outside any component tree.

/** Where an unauthenticated visit is sent, and what it comes back to. */
export const LOGIN_ROUTE = '/login';
/** Where a server with no owner yet sends everybody (Decision 9). */
export const SETUP_ROUTE = '/setup';
/** Where an invitation link lands. */
export const INVITE_ROUTE = '/invite';

/** The three screens that work without a session, and the only ones. */
export const OUTSIDE_THE_SHELL = [LOGIN_ROUTE, SETUP_ROUTE, INVITE_ROUTE];

class Auth {
	#me = $state.raw<Me | null>(null);

	/** The signed-in account, or null when nobody is. */
	get account() {
		return this.#me?.account ?? null;
	}

	get signedIn() {
		return this.#me !== null;
	}

	/** An owner runs the server: every project, and the accounts themselves. */
	get owner() {
		return this.#me?.account.owner ?? false;
	}

	/** What this account can reach, sorted by name, with the role in each. */
	get projects(): readonly Membership[] {
		return this.#me?.projects ?? [];
	}

	/** What to call somebody: the name they chose, else the name they sign in with. */
	get displayName() {
		const account = this.#me?.account;
		return account ? accountName(account) : '';
	}

	adopt(me: Me) {
		this.#me = me;
	}

	/**
	 * Takes the account a write answered with, keeping the projects: the
	 * response to `PATCH /auth/me` is the account and nothing else, and a
	 * second `me` to learn what the first call already said would be a
	 * round trip for nothing (spec 034 #9).
	 */
	revise(account: Account) {
		if (this.#me) this.#me = { ...this.#me, account };
	}

	/** Forgets who was here; the sign-out path and the 401 path both end here. */
	clear() {
		this.#me = null;
	}

	/**
	 * What a 401 means now: the cookie is gone, expired, or belongs to an
	 * account that was disabled or deleted under an open tab. Every screen
	 * reads project data, so there is nothing to stay on — the person is sent
	 * to the login form with where they were, and comes back there.
	 */
	reject() {
		this.clear();
		const here = window.location.pathname + window.location.search;
		if (window.location.pathname === LOGIN_ROUTE) return;
		goto(`${LOGIN_ROUTE}?next=${encodeURIComponent(here)}`, { replaceState: true });
	}

	/**
	 * The token a setup or invitation link carries. It rides in the fragment,
	 * which a browser never puts on the wire, so the link can be pasted into a
	 * chat without the secret reaching a server log on the way.
	 */
	tokenFromFragment(): string | null {
		const hash = window.location.hash;
		if (!hash.startsWith('#')) return null;
		return new URLSearchParams(hash.slice(1)).get('token');
	}

	/**
	 * Takes the token back out of the URL. Replaced rather than pushed: the
	 * link must not survive in history, where a back button or a shared screen
	 * would hand the secret to whoever is looking (spec 006 #8).
	 *
	 * The browser's own `replaceState` rather than SvelteKit's, because this
	 * runs in the first effect of a screen whose router has not finished
	 * starting — where `$app/navigation`'s throws — and because dropping a
	 * fragment is not a navigation the router has anything to record: the
	 * route, the query and the history entry are all exactly what they were.
	 */
	stripFragment() {
		if (!window.location.hash) return;
		const here = window.location.pathname + window.location.search;
		window.history.replaceState(window.history.state, '', here);
	}
}

/**
 * Where the login form sends somebody after they sign in: back to the screen
 * the guard interrupted, and never anywhere else. With nothing to go back to,
 * the project's front page (spec 034 #12).
 *
 * The check is an origin comparison rather than a prefix test, because the URL
 * parser is the only thing that agrees with the URL parser: for a special
 * scheme it treats `/\` exactly like `//`, so `?next=/\elsewhere.example`
 * parses to a different origin while passing any "starts with one slash"
 * rule. Nothing is redirected off-site either way — SvelteKit refuses a
 * cross-origin `goto` — but that refusal would land as a failed sign-in on a
 * reader who is in fact signed in.
 */
export function returnTo(url: URL, fallback = '/dashboard'): string {
	const asked = url.searchParams.get('next');
	if (!asked) return fallback;
	try {
		const target = new URL(asked, url.origin);
		if (target.origin !== url.origin) return fallback;
		// The three screens outside the shell are not somewhere to come back
		// to: `?next=/login` after a sign-in is a loop, and `?next=/invite`
		// lands on a token that has just been spent.
		const path = target.pathname;
		// Measured decoded and without trailing slashes, so `/%6Cogin` and
		// `/login/` are `/login`: the router decodes a path before it
		// matches one and serves both, and the loop is the same loop. A
		// malformed escape throws, and lands on the fallback below.
		const bare = decodeURIComponent(path).replace(/\/+$/, '') || '/';
		if (OUTSIDE_THE_SHELL.includes(bare)) return fallback;
		// Same origin is not enough on its own: `/.//evil.example/x`
		// resolves on this origin to the path `//evil.example/x`, which
		// handed on as a path is a protocol-relative URL to another host
		// (spec 006 #17). No screen here has an empty first segment.
		if (path.startsWith('//')) return fallback;
		return path + target.search;
	} catch {
		return fallback;
	}
}

export const auth = new Auth();
