// Who is annotating (spec 024 #6). It is a signature, not a credential: it
// travels in the body of the writes that finish an item and in the metadata of
// the scores they check.
//
// Signed in, it is the account's name, and nobody types it (spec 048 #11): the
// server already puts the account on every score as its author, and asking the
// same person to type what the server knows is a second source of truth. The
// name asked for once and kept in the browser is what is left for a desk with
// no account behind it.

import { auth } from './auth.svelte';

const STORAGE_KEY = 'tracepad.annotator';

class Annotator {
	#name = $state.raw<string | null>(null);
	/** Whether the stored name has been read yet, so the desk asks once. */
	#restored = false;

	/** The name this browser annotates under, or null when nobody said. */
	get name() {
		if (this.fromAccount) return auth.displayName;
		if (!this.#restored) {
			this.#restored = true;
			this.#name = read();
		}
		return this.#name;
	}

	/** Whether the name is the signed-in account's, which is not changed here. */
	get fromAccount() {
		return auth.displayName !== '';
	}

	/** Stores the name the desk asked for; an empty one is not a name. */
	adopt(name: string): boolean {
		const trimmed = name.trim().slice(0, 200);
		if (!trimmed) return false;
		this.#restored = true;
		this.#name = trimmed;
		write(trimmed);
		return true;
	}
}

// Storage can be switched off, and a private window throws on access. Neither
// is a reason to fail: without it the desk asks again next time.
function read(): string | null {
	try {
		return window.localStorage.getItem(STORAGE_KEY);
	} catch {
		return null;
	}
}

function write(value: string) {
	try {
		window.localStorage.setItem(STORAGE_KEY, value);
	} catch {
		// Nothing to do: the name lasts this tab.
	}
}

export const annotator = new Annotator();
