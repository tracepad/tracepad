import { api, type Project } from '$lib/api/client.svelte';

// The project the credential belongs to, which is all the shell needs to say
// whose data is on screen. `GET /api/v1/projects` answers with exactly one
// entry for a project key, and a project key is the only credential that gets
// past login (spec 006 #13) — so there is nothing to pick between here.

class CurrentProject {
	#project = $state.raw<Project | null>(null);
	#requested = false;

	get name() {
		return this.#project?.name ?? null;
	}

	/**
	 * Loads the name once per signed-in session. A failure is swallowed: the
	 * name is a label, and a screen that refused to render because it could
	 * not decorate its sidebar would be a worse answer than an unlabelled one.
	 */
	async load() {
		if (this.#requested) return;
		this.#requested = true;
		try {
			const { projects } = await api.listProjects();
			this.#project = projects[0] ?? null;
		} catch {
			this.#requested = false;
		}
	}

	forget() {
		this.#project = null;
		this.#requested = false;
	}
}

export const project = new CurrentProject();
