<script lang="ts">
	import { api, type DryRun, type Project } from '$lib/api/client.svelte';
	import ConfirmCard from '../ConfirmCard.svelte';
	import Card from './Card.svelte';

	// Erasing one end user's data (spec 005 #7): the traces filed under that
	// user id, their observations, payloads and scores. Raw OTLP bodies are
	// deliberately untouched — they are an archive on their own schedule, and
	// the server's own note says so in the preview rather than this screen
	// claiming otherwise.
	//
	// The echo here is the user id, because the user is what is being erased.

	let { current }: { current: Project } = $props();

	let userID = $state('');
	const target = $derived(userID.trim());

	async function erase(confirm?: string): Promise<DryRun | string> {
		const answer = await api.eraseUserData(current.id, target, confirm);
		if ('dry_run' in answer && answer.dry_run) return answer as DryRun;
		const deleted = (answer as { deleted: Record<string, number> }).deleted;
		return `Erased ${deleted.traces ?? 0} traces belonging to ${target}.`;
	}
</script>

<Card
	title="Danger zone"
	description="Irreversible operations on this project's data. Each one shows what it would remove
		before it removes anything."
>
	<ConfirmCard
		title="Erase everything about one user"
		description="Answers a deletion request: every trace filed under this user id, with its
			observations, payloads and scores."
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
</Card>
