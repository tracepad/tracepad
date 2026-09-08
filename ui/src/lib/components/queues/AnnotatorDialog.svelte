<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { annotator } from '$lib/annotator.svelte';
	import Button from '../Button.svelte';

	// Who is annotating (spec 024 #6, #12). The desk asks once and the browser
	// keeps the answer; the header says who it thinks you are and opens this
	// again, because a shared machine is exactly where the wrong name gets
	// written into forty verdicts.
	//
	// It is a signature, not a credential: the store has no users and this
	// spec does not invent them. What it buys is a team being able to read
	// "who said this", which is all a small team needs.

	// `onclose` says only that the dialog is shut. Whether a name was adopted
	// is `annotator.name`, which the caller has to branch on anyway: the
	// dialog can be dismissed with Escape or the overlay, and a desk that
	// believed a close meant a name sat on its spinner for ever (found in
	// review).
	let { open = false, onclose }: { open?: boolean; onclose: () => void } = $props();

	let name = $state('');

	$effect(() => {
		if (open) name = annotator.name ?? '';
	});

	function keep() {
		if (annotator.adopt(name)) onclose();
	}
</script>

<Dialog.Root {open} onOpenChange={(next) => !next && onclose()}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content
			class="border-border bg-canvas shadow-overlay fixed top-1/2 left-1/2 z-50
				w-[min(24rem,calc(100vw-1.5rem))] -translate-x-1/2 -translate-y-1/2 rounded-lg border p-4"
		>
			<Dialog.Title class="text-lg font-semibold">Who is reviewing?</Dialog.Title>
			<Dialog.Description class="text-muted mt-1 text-sm">
				This name is written beside every verdict you complete, so the team can see who said
				what. It stays in this browser.
			</Dialog.Description>

			<label for="annotator-name" class="mt-3 mb-1 block text-xs font-medium">Name</label>
			<input
				id="annotator-name"
				name="annotator"
				type="text"
				bind:value={name}
				placeholder="ada"
				autocomplete="off"
				spellcheck="false"
				onkeydown={(event) => event.key === 'Enter' && keep()}
				class="border-border bg-canvas text-fg w-full rounded-md border px-2 py-1 text-sm"
			/>

			<div class="mt-4 flex justify-end gap-1.5">
				<Button variant="primary" disabled={name.trim() === ''} onclick={keep}>Start</Button>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
