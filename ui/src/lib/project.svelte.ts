import { api, type Project } from '$lib/api/client.svelte';
import { auth } from '$lib/auth.svelte';

// The project the credential belongs to, which is all the shell needs to say
// whose data is on screen. `GET /api/v1/projects` answers with exactly one
// entry for a project key, and a project key is the only credential that gets
// past login (spec 006 #13) — so there is nothing to pick between here.

class CurrentProject {
	#project = $state.raw<Project | null>(null);
	/**
	 * The credential the name belongs to, not merely "have we asked yet". A
	 * 401 signs the reader out without going through the sidebar's sign-out,
	 * so the next key may be a different project's — and a boolean would leave
	 * the old project's name in the sidebar for the rest of the session.
	 */
	#loadedFor: string | null = null;

	get name() {
		return this.#project?.name ?? null;
	}

	/**
	 * Loads the name once per credential. A failure is swallowed: the name is
	 * a label, and a screen that refused to render because it could not
	 * decorate its sidebar would be a worse answer than an unlabelled one.
	 */
	async load() {
		const key = auth.key;
		if (!key || this.#loadedFor === key) return;
		this.#loadedFor = key;
		this.#project = null;
		try {
			const { projects } = await api.listProjects();
			// A key that changed again while this was in flight owns the
			// sidebar now; this answer is about somebody else's project.
			if (this.#loadedFor !== key) return;
			this.#project = projects[0] ?? null;
		} catch {
			if (this.#loadedFor === key) this.#loadedFor = null;
		}
	}

	forget() {
		this.#project = null;
		this.#loadedFor = null;
	}
}

export const project = new CurrentProject();
