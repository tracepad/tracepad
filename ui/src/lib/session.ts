import { api } from '$lib/api/client.svelte';
import { auth } from '$lib/auth.svelte';
import { project } from '$lib/project.svelte';

// What the shell learns before it renders anything (spec 028, Application
// contract): whether this server has an owner yet, and who is signed in. Two
// requests in parallel, because they are independent questions and the guard
// needs both answers at once.
//
// This module is where the client and the session state meet, so that neither
// has to import the other: the client tells `auth` that a 401 happened, `auth`
// holds what `me` answered, and nothing in either of them knows how to make
// the request that fills it.

/** Asks the server the two questions the guard decides on. */
export async function bootstrap(): Promise<{ setupRequired: boolean }> {
	const [required, me] = await Promise.all([needsSetup(), api.me().catch(() => null)]);
	if (me) {
		auth.adopt(me);
		project.restore();
	}
	return { setupRequired: required };
}

/**
 * Whether this server still has nobody who can sign in (Decision 9).
 *
 * The guard asks again rather than trusting what it learned at boot, because
 * that answer goes stale the moment somebody uses the setup screen — in this
 * tab or in another one. Only a server that still has no owner ever pays for
 * the question; the first `false` ends it for the life of the page.
 *
 * A server that cannot be reached is not a server that needs setting up: the
 * login form is where "cannot reach the server" is worth saying, and it says
 * it on the attempt rather than on the way in.
 */
export async function needsSetup(): Promise<boolean> {
	try {
		return (await api.getSetup()).required;
	} catch {
		return false;
	}
}

/**
 * Reads the session again after something changed what it says: a project
 * renamed, a role granted, a display name edited. One call, because `me` is
 * the one place the shell reads any of it from (Decision 15).
 */
export async function refresh() {
	auth.adopt(await api.me());
}

/** Signs in: the cookie is already set, so all that is left is to read `me`. */
export async function begin() {
	await refresh();
	project.restore();
}

/** Ends the session here and forgets what it was showing. */
export async function end() {
	try {
		await api.logout();
	} catch {
		// A sign-out that the server did not hear is still a sign-out here:
		// the alternative is a person stuck on a screen they asked to leave.
	}
	auth.clear();
	project.forget();
}
