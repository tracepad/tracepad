import type { Key } from '$lib/api/client.svelte';
import { timestamp } from '$lib/format';

// What the Keys card says about who minted a key (spec 045 #8, #14). The
// server computes the minter's standing when the listing is read; this only
// turns it into words.

type Minter = Key['created_by'];

/** Who minted a key, for a table cell. */
export function minter(by: Minter): string {
	switch (by.kind) {
		case 'account':
			return `${by.email} (${by.standing})`;
		case 'admin_token':
			return 'the admin token';
		case 'startup':
			return 'the server';
	}
	return 'unknown';
}

/** When a key last authenticated a request, or "never" until it has (spec 045 #9). */
export function lastUse(at: string | null | undefined): string {
	return at ? timestamp(at) : 'never';
}

/** A key's name is at most this many characters — code points, as the server counts. */
export const MAX_KEY_NAME = 64;

/** Whether a name is too long for a key, counted the way the server counts it. */
export function tooLong(name: string): boolean {
	return [...name.trim()].length > MAX_KEY_NAME;
}

/**
 * Whether the person who minted a key can no longer manage keys here: the key
 * outlived their access, which no revocation followed (spec 045 #10).
 */
export function outlived(by: Minter): boolean {
	return by.kind === 'account' && by.standing !== 'owner' && by.standing !== 'editor';
}
