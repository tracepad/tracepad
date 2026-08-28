// The admin token (spec 007 #3), which is a second credential and is kept like
// one: its own storage key, its own lifetime, and — the part that matters —
// its own set of endpoints.
//
// It is entered on the Settings screen rather than at the login form, because
// the login form's job is to admit somebody to the screens, and this token
// cannot read a single trace (spec 006 #13 is unchanged). What it unlocks is
// project lifecycle, and that is where it is asked for.
//
// It is never sent anywhere but the administrative endpoints that demand it.
// The API client takes an explicit scope per request for exactly that reason,
// and a fetch-spy test holds the line.

const STORAGE_KEY = 'tracepad.admin';

class AdminToken {
	#token = $state.raw<string | null>(null);

	get token() {
		return this.#token;
	}

	/** Whether the Administration section is open. */
	get unlocked() {
		return this.#token !== null;
	}

	/** Reads the token a previous visit left behind, if any. */
	restore() {
		this.#token = read();
	}

	/** Stores a token the server has already accepted. */
	adopt(token: string): boolean {
		const trimmed = token.trim();
		if (!trimmed) return false;
		this.#token = trimmed;
		write(trimmed);
		return true;
	}

	/** Locks the section again: on sign-out, and on a 401 from the token. */
	clear() {
		this.#token = null;
		write(null);
	}
}

// Storage can be switched off, and a private window throws on access. Neither
// is a reason to fail: without it the token simply lasts one session.
function read(): string | null {
	try {
		return window.localStorage.getItem(STORAGE_KEY);
	} catch {
		return null;
	}
}

function write(value: string | null) {
	try {
		if (value === null) window.localStorage.removeItem(STORAGE_KEY);
		else window.localStorage.setItem(STORAGE_KEY, value);
	} catch {
		// Nothing to do: the section stays unlocked for this tab only.
	}
}

export const admin = new AdminToken();
