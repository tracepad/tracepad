import { page } from '$app/state';
import type { Membership } from '$lib/api/client.svelte';
import { auth } from '$lib/auth.svelte';
import { CURSOR, DIRECTION } from '$lib/page';
import { OBS, PEEK, TRACE } from '$lib/peek';
import { PREFIX, under, within } from '$lib/paths';
import { SCREENS } from '$lib/screens';

// Which project is on screen: the one in the URL (spec 029 #2). Every screen
// inside the shell lives under `/p/{id}` (#1), so the answer is a route
// parameter and not a store — back, forward, a reload and a pasted link all
// agree because there is no second state to fall behind. `me.projects` says
// what the id *is* (name, role) and whether the account can reach it; it no
// longer says which project to show.
//
// What is still kept, per account rather than per browser, is the id a bare
// path redirects to (#3): the last project this account looked at, so a
// bookmark from before the prefix and the `/` the server prints both land
// where the person was — and never on another person's project.

const STORAGE_PREFIX = 'tracepad.project.';

class CurrentProject {
	/**
	 * The project in force: the one the URL names, while `me.projects` holds
	 * it. An id the account cannot reach — unknown, malformed, or a role
	 * taken away under an open tab — reads as nothing rather than pointing
	 * the header at a project the server would refuse (#4).
	 */
	get current(): Membership | null {
		const id = page.params.project;
		if (!id) return null;
		return auth.projects.find((one) => one.id === id) ?? null;
	}

	get id() {
		return this.current?.id ?? null;
	}

	get name() {
		return this.current?.name ?? null;
	}

	/** What this account may do here. `owner` is every project (spec 028 #2). */
	get role() {
		return this.current?.role ?? null;
	}

	/** Whether the editing controls are rendered at all (spec 028 #15). */
	get editor() {
		return this.role === 'editor' || this.role === 'owner';
	}

	/**
	 * The project a bare path is sent to (#3): the remembered one while the
	 * account can still reach it, otherwise the first of `me.projects` by
	 * name, and nothing at all for an account that reaches none.
	 */
	remembered(): string | null {
		const projects = auth.projects;
		const account = auth.account?.id;
		const kept = account ? read(STORAGE_PREFIX + account) : null;
		return projects.find((one) => one.id === kept)?.id ?? projects[0]?.id ?? null;
	}

	/** Opening a screen under `/p/{id}` remembers `{id}` for the next bare path. */
	remember(id: string) {
		const account = auth.account?.id;
		if (account) write(STORAGE_PREFIX + account, id);
	}
}

/**
 * The one way the interface writes a link (#2): the path under the project on
 * screen, plus a query if one is given. Every row link, every `goto`, the
 * sidebar and the settings tabs go through here, so the prefix has one place
 * to be right. Outside a project — the no-projects screen, an error page — the
 * path stays bare, and the redirect of #3 takes it wherever it should go.
 */
export function href(path: string, query: string | URLSearchParams = ''): string {
	const search = query instanceof URLSearchParams ? query.toString() : query.replace(/^\?/, '');
	const target = search ? `${path}${path.includes('?') ? '&' : '?'}${search}` : path;
	const id = page.params.project;
	return id ? under(target, id) : target;
}

/**
 * Where a bare path is sent (#3): the same path and query under the remembered
 * project, or `/p` — the no-projects screen — when there is nothing to
 * remember.
 */
export function bareTarget(path: string, search = ''): string {
	const id = project.remembered();
	return id ? under(path + search, id) : PREFIX;
}

/**
 * The segments a switch keeps (#6): every screen the navigation lists
 * (`screens.ts`), so a screen added there is kept without being named again.
 * `stats` stays for its redirect to the dashboard (spec 034 #1).
 */
const KEPT_SEGMENTS = new Set([...SCREENS.map((screen) => screen.href.slice(1)), 'stats']);

/**
 * Where switching to another project lands (#6): the same section, and for
 * Settings the same tab, with everything deeper dropped — a trace, a prompt, a
 * queue belong to the project they came from. The query is kept except the
 * keys that name something in the old project: the page's position and the
 * peek panel. A filter is a question, and the same question of the other
 * project; a cursor is an answer. A first segment that is no section — the
 * 404 a stale link lands on — is not carried over: the switcher is a way out
 * of that page, not a way to see it again under another id. Nor is a screen
 * that is under no project at all — `/p`, the bare Account tab (#14): there
 * is no section to keep, and the switch lands on the project's dashboard
 * (spec 034 #1).
 */
export function switchTarget(url: URL, id: string): string {
	if (within(url.pathname) === url.pathname) return under('/dashboard', id);
	const segments = within(url.pathname).split('/').filter(Boolean);
	const section = segments[0] && KEPT_SEGMENTS.has(segments[0]) ? segments[0] : 'dashboard';
	const kept = section === 'settings' && segments[1] ? `/${section}/${segments[1]}` : `/${section}`;
	const query = new URLSearchParams(url.search);
	for (const key of [CURSOR, DIRECTION, PEEK, TRACE, OBS]) query.delete(key);
	const search = query.toString();
	return under(search ? `${kept}?${search}` : kept, id);
}

// Storage is a browser feature a person can switch off, and a private window
// throws on access rather than returning nothing. Neither is a reason for the
// app to fail to load: without it a bare path lands on the first project.
function read(name: string): string | null {
	try {
		return window.localStorage.getItem(name);
	} catch {
		return null;
	}
}

function write(name: string, value: string) {
	try {
		window.localStorage.setItem(name, value);
	} catch {
		// Nothing to do and nothing worth saying: the app works, the choice
		// just will not outlive the tab.
	}
}

export { under };
export const project = new CurrentProject();

/**
 * The switcher's menu, which the not-there screen opens (#4): the person's
 * next move is to pick a project that is theirs, so the list is put in front
 * of them rather than behind a click.
 */
export const switcher = $state({ open: false });
