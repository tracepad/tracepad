import type { Key } from '$lib/api/client.svelte';

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

export type Scope = Key['scopes'][number];

/**
 * The three scopes a key may hold, in the server's order, each with the line
 * that says what it lets a key do (spec 045 #1).
 */
export const SCOPES: { scope: Scope; does: string }[] = [
	{
		scope: 'ingest',
		does: 'send spans, use the Langfuse media channel, write scores — what a running application does'
	},
	{ scope: 'read', does: "every read of the project's data, and nothing that changes it" },
	{
		scope: 'write',
		does: 'every change a key may make — prompts, datasets, runs, queues, deleting traces, retention, erasure'
	}
];
