import type { Membership } from '$lib/api/client.svelte';
import { auth } from '$lib/auth.svelte';

// Which project is on screen. A session is not a project the way a key was
// (spec 028 #6), so something has to say which one every request is about — and
// until spec 029 puts it in the URL with a switcher, it is the first of
// `me.projects` by name (Decision 13).
//
// Remembered per account rather than per browser: two people who share a
// laptop must not swap each other's project, and the id of a project one of
// them cannot reach would be answered `403` for the other.

const STORAGE_PREFIX = 'tracepad.project.';

class CurrentProject {
	#chosen = $state.raw<string | null>(null);

	/**
	 * The project in force: the remembered one while it is still reachable,
	 * otherwise the first by name. A role taken away under an open tab drops
	 * the row out of `me.projects`, and the answer falls back rather than
	 * pointing the header at a project the server would refuse.
	 */
	get current(): Membership | null {
		const projects = auth.projects;
		return projects.find((one) => one.id === this.#chosen) ?? projects[0] ?? null;
	}

	get id() {
		return this.current?.id ?? null;
	}

	get name() {
		return this.current?.name ?? null;
	}

	/** What this account may do here. `owner` is every project (Decision 2). */
	get role() {
		return this.current?.role ?? null;
	}

	/** Whether the editing controls are rendered at all (Decision 15). */
	get editor() {
		return this.role === 'editor' || this.role === 'owner';
	}

	/** Reads back the project this account was last looking at. */
	restore() {
		const id = auth.account?.id;
		this.#chosen = id ? read(STORAGE_PREFIX + id) : null;
	}

	choose(id: string) {
		this.#chosen = id;
		const account = auth.account?.id;
		if (account) write(STORAGE_PREFIX + account, id);
	}

	forget() {
		this.#chosen = null;
	}
}

// Storage is a browser feature a person can switch off, and a private window
// throws on access rather than returning nothing. Neither is a reason for the
// app to fail to load: without it the choice simply lasts one session.
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

export const project = new CurrentProject();
