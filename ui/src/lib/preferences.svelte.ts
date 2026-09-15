import { ApiError, api } from '$lib/api/client.svelte';
import { auth } from '$lib/auth.svelte';
import { arrangementOf, withArrangement, type Arrangement } from '$lib/dashboard';

// The dashboard's arrangement, read from and written to the account's
// preferences (spec 034 #9). The account is read once on load with the rest
// of `me`; a write sends the whole object back and takes the account the
// server answers with, so a tab knows what it holds after its own writes and
// never polls for anybody else's.

/** The arrangement this account keeps for a project, made whole. */
export function dashboard(projectId: string): Arrangement {
	return arrangementOf(auth.account?.preferences ?? {}, projectId);
}

/**
 * Writes an arrangement — or removes the project's key, which is Reset —
 * and answers with the failure to show, or null. A failure leaves `auth`
 * alone: the arrangement on screen stays what the person made, and the
 * message says why the server did not keep it.
 */
export async function setDashboard(
	projectId: string,
	arrangement: Arrangement | null
): Promise<string | null> {
	const preferences = withArrangement(auth.account?.preferences ?? {}, projectId, arrangement);
	try {
		const { account } = await api.patchMe({ preferences });
		auth.revise(account);
		return null;
	} catch (cause) {
		return cause instanceof ApiError ? cause.message : 'The arrangement could not be saved.';
	}
}
