<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { goto } from '$app/navigation';
	import { api, type DryRun } from '$lib/api/client.svelte';
	import { count } from '$lib/format';
	import ConfirmCard from '../ConfirmCard.svelte';

	// Deleting a prompt takes a name's whole history (spec 021 #7), so it wears
	// the ceremony every act with a blast radius wears in this interface: the
	// server's own dry run on screen, and the name typed back (spec 005 #8).
	//
	// The endpoint answers in its own shape; translating it here rather than
	// teaching the card a second one keeps one confirmation card in the
	// interface, exactly as the dataset deletion does.

	let { open = false, name, onclose }: { open?: boolean; name: string; onclose: () => void } =
		$props();

	async function ask(confirm?: string): Promise<DryRun | string> {
		const answer = await api.deletePrompt(name, confirm);
		if (!answer.dry_run) {
			return `Deleted ${answer.name}: ${count(answer.would_delete.versions)} versions and ${count(answer.would_delete.labels)} labels gone.`;
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
				Shows what deleting this prompt would remove and asks for its name.
			</Dialog.Description>
			<ConfirmCard
				title="Delete {name}"
				description="Every version of this prompt and every label pointing into it. Versions are never
					deleted alone, so there is no smaller act than this one — and anything fetching the name
					by label stops resolving."
				echoLabel="prompt name"
				previewLabel="Show what would go"
				executeLabel="Delete this prompt"
				subject={name}
				preview={() => ask()}
				execute={(confirm) => ask(confirm) as Promise<string>}
				ondone={() => void goto('/prompts')}
			/>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
