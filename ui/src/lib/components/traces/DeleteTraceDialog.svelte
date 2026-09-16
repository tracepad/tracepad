<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { api, type DryRun } from '$lib/api/client.svelte';
	import { count } from '$lib/format';
	import ConfirmCard from '../ConfirmCard.svelte';

	// *Delete…* on the trace header (spec 035 #8): the reader's gesture at the
	// surface where the reading happens, wearing the ceremony every destructive
	// act wears (spec 005 #8, spec 007 #5) — the server's dry run on screen,
	// and *Delete this trace* under it. The echo is the id, prefilled: it is
	// on screen already, and typing thirty-two hex characters back is a
	// ritual, not a check. The server checks it either way.

	let {
		open = false,
		traceID,
		onclose,
		/** The host's own after: close the peek and re-read, or leave the page. */
		ondeleted
	}: { open?: boolean; traceID: string; onclose: () => void; ondeleted: () => void } = $props();

	async function ask(confirm?: string): Promise<DryRun | string> {
		const answer = await api.deleteTrace(traceID, confirm);
		if (answer.dry_run) return answer;
		return `Deleted the trace: ${count(answer.deleted.observations)} observations, ${count(answer.deleted.scores)} scores.`;
	}
</script>

<Dialog.Root {open} onOpenChange={(next) => !next && onclose()}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content
			class="fixed top-1/2 left-1/2 z-50 max-h-[90dvh] w-[min(32rem,calc(100vw-1.5rem))]
				-translate-x-1/2 -translate-y-1/2 overflow-y-auto"
		>
			<Dialog.Title class="sr-only">Delete this trace</Dialog.Title>
			<Dialog.Description class="sr-only">
				Shows what deleting this trace would remove and asks to confirm it.
			</Dialog.Description>
			<ConfirmCard
				title="Delete this trace"
				description="The trace with its observations, scores, payloads and queue items. The hour it
					started in is corrected in the statistics; a raw OTLP body it arrived in is not touched."
				echoLabel="trace id"
				previewLabel="Show what would go"
				executeLabel="Delete this trace"
				subject={traceID}
				immediate
				prefill
				preview={() => ask()}
				execute={(confirm) => ask(confirm) as Promise<string>}
				ondone={ondeleted}
			/>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
