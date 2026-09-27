import { ApiError, type AccountDetail, type Membership } from '$lib/api/client.svelte';

// The few things about an account that more than one screen needs to say the
// same way (spec 028): what a password has to be, what the server said when it
// refused, and what standing an account is in.

/**
 * Decision 1: ten characters and a length cap, and no other rule. The cap is
 * in bytes of UTF-8, because that is what `bcrypt` reads (Decision 31).
 */
export const MIN_PASSWORD = 10;
export const MAX_PASSWORD_BYTES = 72;

/** What the cap and the floor say, for the hint under a new password. */
export const PASSWORD_HINT = `At least ${MIN_PASSWORD} characters and at most ${MAX_PASSWORD_BYTES} bytes. There is no other rule.`;

/**
 * What is wrong with a new password and its confirmation, if anything. The
 * server checks all of this again — this is here so that a typo in the second
 * field costs a glance rather than a round trip.
 */
export function passwordProblem(password: string, again: string): string | null {
	if (password.length < MIN_PASSWORD) {
		return `A password is at least ${MIN_PASSWORD} characters. There is no other rule.`;
	}
	if (new TextEncoder().encode(password).length > MAX_PASSWORD_BYTES) {
		return `A password is at most ${MAX_PASSWORD_BYTES} bytes; a character outside plain ASCII takes two to four.`;
	}
	if (password !== again) return 'The two passwords do not match.';
	return null;
}

/**
 * The server's own words, verbatim, wherever there are any (spec 007 #4): it
 * knows why it refused, and a paraphrase would be this screen's opinion of why.
 */
export function said(cause: unknown, fallback: string): string {
	return cause instanceof ApiError ? cause.message : fallback;
}

/** Where an account stands: invited and not yet in, in, or shut out. */
export function standing(account: AccountDetail): 'pending' | 'active' | 'disabled' {
	if (account.disabled) return 'disabled';
	return account.pending ? 'pending' : 'active';
}

/** What an account reaches, for a table cell: every project, or the list. */
export function reaches(account: AccountDetail): string {
	if (account.owner) return 'every project';
	if (account.projects.length === 0) return 'none';
	return account.projects.map((one) => `${one.name} (${one.role})`).join(', ');
}

/** The role of one project for an account, or none. */
export function roleIn(projects: readonly Membership[], id: string): string | null {
	return projects.find((one) => one.id === id)?.role ?? null;
}
