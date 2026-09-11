<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { goto } from '$app/navigation';
	import { api, type DryRun } from '$lib/api/client.svelte';
	import { count } from '$lib/format';
	import { href } from '$lib/project.svelte';
	import ConfirmCard from '../ConfirmCard.svelte';

	// Deleting a queue is the act here with a blast radius (spec 024 #8): the
	// list and every item in it. So it wears the ceremony every destructive act
	// in this interface wears — the server's dry run, on screen, and the name
	// typed back (spec 005 #8, spec 007 #5).
	//
	// The note is the half a reader needs: the scores written while annotating
	// stay. They are the product of the work and they are attached to the
	// trace, not to the queue (#3).

	let { open = false, name, onclose }: { open?: boolean; name: string; onclose: () => void } =
		$props();

	async function ask(confirm?: string): Promise<DryRun | string> {
		const answer = await api.deleteQueue(name, confirm);
		if (!answer.dry_run) {
			return `Deleted ${answer.name}: ${count(answer.would_delete.items)} items. The scores stay.`;
		}
		return {
			dry_run: true,
			would_delete: answer.would_delete,
			confirm: answer.confirm ?? answer.name,
			note: answer.note
		};
	}
</script>

<Dialog.Root {open} onOpenChange={(next) => !next && onclose()}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content
			class="fixed top-1/2 left-1/2 z-50 max-h-[90dvh] w-[min(32rem,calc(100vw-1.5rem))]
				-translate-x-1/2 -translate-y-1/2 overflow-y-auto"
		>
			<Dialog.Title class="sr-only">Delete {name}</Dialog.Title>
			<Dialog.Description class="sr-only">
				Shows what deleting this queue would remove and asks for its name.
			</Dialog.Description>
			<ConfirmCard
				title="Delete {name}"
				description="The list and every item in it. The scores written while annotating stay on
					their traces — they are the work, and this was only the list of what to do."
				echoLabel="queue name"
				previewLabel="Show what would go"
				executeLabel="Delete this queue"
				subject={name}
				preview={() => ask()}
				execute={(confirm) => ask(confirm) as Promise<string>}
				ondone={() => void goto(href('/queues'))}
			/>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
