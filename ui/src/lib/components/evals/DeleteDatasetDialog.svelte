<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { goto } from '$app/navigation';
	import { api, type DryRun } from '$lib/api/client.svelte';
	import { count } from '$lib/format';
	import ConfirmCard from '../ConfirmCard.svelte';

	// Deleting a dataset is the one act here with a blast radius (spec 016 #6):
	// every version of every item and every run go with it. So it is the same
	// ceremony every destructive act in this interface wears — the server's dry
	// run, on screen, and the name typed back (spec 005 #8, spec 007 #5).
	//
	// The endpoint answers in its own shape (`DatasetDeletion`), which is the
	// dry-run contract with the counts named after the nouns rather than in a
	// map. Translating it here — instead of teaching the card a second shape —
	// keeps one confirmation card in the interface.

	let { open = false, name, onclose }: { open?: boolean; name: string; onclose: () => void } =
		$props();

	async function ask(confirm?: string): Promise<DryRun | string> {
		const answer = await api.deleteDataset(name, confirm);
		if (!answer.dry_run) {
			return `Deleted ${answer.dataset}: ${count(answer.items)} items and ${count(answer.runs)} runs.`;
		}
		return {
			dry_run: true,
			// The pinned traces are not deleted, so they are not in this list;
			// they are what the note is about.
			would_delete: { items: answer.items, runs: answer.runs },
			confirm: answer.confirm ?? answer.dataset,
			note: `${count(answer.pinned_traces)} pinned traces: ${answer.note ?? ''}`.trim()
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
				Shows what deleting this dataset would remove and asks for its name.
			</Dialog.Description>
			<ConfirmCard
				title="Delete {name}"
				description="Every version of every item and every run of this dataset. The traces those runs
					were keeping are not deleted — they stop being pinned and live as long as retention says."
				echoLabel="dataset name"
				previewLabel="Show what would go"
				executeLabel="Delete this dataset"
				subject={name}
				preview={() => ask()}
				execute={(confirm) => ask(confirm) as Promise<string>}
				ondone={() => void goto('/datasets')}
			/>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
