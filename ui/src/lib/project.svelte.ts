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
	 * The whole project row, which the Settings screen needs: the id every
	 * management endpoint is addressed by, and the retention windows it edits.
	 */
	get current() {
		return this.#project;
	}

	/**
	 * Loads the name once per credential. A failure is swallowed: the name is
	 * a label, and a screen that refused to render because it could not
	 * decorate its sidebar would be a worse answer than an unlabelled one.
	 */
	async load() {
		const key = auth.key;
		if (!key || this.#loadedFor === key) return;
		// A different credential means a possibly different project, so the
		// old row goes now rather than lingering under the new key.
		this.#project = null;
		await this.#read(key);
	}

	/**
	 * Reads the project again after something changed it. The sidebar and the
	 * Settings screen show the same row, so a rename has to reach both.
	 *
	 * Unlike `load`, this does not blank the row first. The Settings screen
	 * renders its cards only when there is a project, so a momentary null
	 * would tear all of them down and build them again — losing, among other
	 * things, the "done" the card had just put on the screen.
	 */
	async refresh() {
		const key = auth.key;
		if (!key) return;
		await this.#read(key);
	}

	async #read(key: string) {
		this.#loadedFor = key;
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
