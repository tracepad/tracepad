// Who is annotating (spec 024 #6). The store has no users and this spec does
// not invent them: the desk asks for a name once and keeps it in the browser,
// which is what a small team needs to read "who said this" and nothing it
// could not fake by other means.
//
// Kept like the two credentials beside it — its own key, its own lifetime —
// but it is not a credential: it is a signature, and it travels in the body of
// the writes that finish an item and in the metadata of the scores they check.

const STORAGE_KEY = 'tracepad.annotator';

class Annotator {
	#name = $state.raw<string | null>(null);
	/** Whether the stored name has been read yet, so the desk asks once. */
	#restored = false;

	/** The name this browser annotates under, or null when nobody said. */
	get name() {
		if (!this.#restored) {
			this.#restored = true;
			this.#name = read();
		}
		return this.#name;
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
