<script lang="ts">
	import { api, type DryRun, type Project } from '$lib/api/client.svelte';
	import { erased, stillRunning } from '$lib/erasure';
	import ConfirmCard from '../ConfirmCard.svelte';
	import Card from './Card.svelte';
	import ViewerNote from './ViewerNote.svelte';

	// Erasing one end user's data (spec 005 #7, spec 044 #1): the traces filed
	// under that user id with what hangs off them, the scores on their
	// sessions, the dataset items cut from them, and their spans inside the
	// raw archive. What the erasure cannot reach is the server's to say, in
	// the preview's note, rather than this screen's.
	//
	// The echo here is the user id, because the user is what is being erased.

	let { current, readOnly = false }: { current: Project; readOnly?: boolean } = $props();

	let userID = $state('');
	const target = $derived(userID.trim());

	async function erase(confirm?: string): Promise<DryRun | string> {
		let answer;
		try {
			answer = await api.eraseUserData(current.id, target, confirm);
		} catch (cause) {
			const running = confirm === undefined ? null : stillRunning(cause, target);
			if (running) return running;
			throw cause;
		}
		if ('dry_run' in answer && answer.dry_run) return answer as DryRun;
		return erased(target, (answer as { deleted: Record<string, number> }).deleted);
	}
</script>

<Card
	title="Danger zone"
	description="Irreversible operations on this project's data. Each one shows what it would remove
		before it removes anything."
>
	{#if readOnly}
		<ViewerNote what="there is nothing here you can run" />
	{:else}
		<ConfirmCard
			title="Erase everything about one user"
			description="Answers a deletion request: every trace filed under this user id, with its
				observations, payloads and scores, the scores on its sessions, the dataset items cut from
				it, and its spans in the raw archive."
			echoLabel="user id"
			previewLabel="Show what would go"
			executeLabel="Erase this user's data"
			subject={target}
			ready={target !== ''}
			preview={() => erase()}
			execute={(confirm) => erase(confirm) as Promise<string>}
		>
			<label for="erase-user" class="text-muted mb-1 block text-xs font-medium">User id</label>
			<input
				id="erase-user"
				type="text"
				bind:value={userID}
				placeholder="user-4821"
				autocomplete="off"
				spellcheck="false"
				class="border-border bg-canvas placeholder:text-subtle w-full max-w-sm rounded-md border
					px-2 py-1 font-mono text-sm"
			/>
		</ConfirmCard>
	{/if}
</Card>
