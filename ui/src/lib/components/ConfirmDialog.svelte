<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { AlertDialog } from 'bits-ui';
	import { ApiError } from '$lib/api/client.svelte';
	import Button from './Button.svelte';

	// The cheap destruction (spec 016 #6): a dialog that names the consequence
	// and asks once. It is deliberately *not* `ConfirmCard` — the echo ceremony
	// belongs to the act with a blast radius, and these endpoints take no
	// `confirm` at all (spec 014 #20), so a dry-run round trip would be theatre
	// the server never agreed to. What is left is the one thing a mis-click
	// needs: a sentence saying what goes, and a second button.
	//
	// The act runs inside the dialog rather than on the way out of it: a
	// deletion that fails must leave the reader where they can read why and
	// press again, which an `AlertDialog.Action` — which closes on click —
	// would not.

	let {
		open = false,
		title,
		/** What happens, in the terms the reader would ask about. */
		description,
		confirmLabel,
		/** Runs the act; a rejection keeps the dialog open with the reason. */
		onconfirm,
		onclose
	}: {
		open?: boolean;
		title: string;
		description: string;
		confirmLabel: string;
		onconfirm: () => Promise<void>;
		onclose: () => void;
	} = $props();

	let busy = $state(false);
	let failure = $state<string | null>(null);

	// A dialog opened again is a fresh question: whatever the last attempt
	// failed with is not about this one.
	$effect(() => {
		if (open) failure = null;
	});

	async function confirm() {
		busy = true;
		failure = null;
		try {
			await onconfirm();
			onclose();
		} catch (cause) {
			// The server's own words (spec 007 #4).
			failure = cause instanceof ApiError ? cause.message : 'The request failed.';
		} finally {
			busy = false;
		}
	}
</script>

<AlertDialog.Root {open} onOpenChange={(next) => !next && !busy && onclose()}>
	<AlertDialog.Portal>
		<AlertDialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<AlertDialog.Content
			class="border-border bg-canvas shadow-overlay fixed top-1/2 left-1/2 z-50 max-h-[90dvh]
				w-[min(28rem,calc(100vw-1.5rem))] -translate-x-1/2 -translate-y-1/2 overflow-y-auto
				rounded-lg border p-4"
		>
			<AlertDialog.Title class="text-lg font-semibold">{title}</AlertDialog.Title>
			<AlertDialog.Description class="text-muted mt-1 text-sm">
				{description}
			</AlertDialog.Description>

			{#if failure}
				<p role="alert" class="text-danger mt-3 flex items-start gap-2 text-sm">
					<TriangleAlert class="mt-0.5 size-4 shrink-0" />
					{failure}
				</p>
			{/if}

			<div class="mt-4 flex justify-end gap-1.5">
				<AlertDialog.Cancel>
					{#snippet child({ props })}
						<Button {...props} disabled={busy}>Cancel</Button>
					{/snippet}
				</AlertDialog.Cancel>
				<Button
					variant="primary"
					class="border-danger bg-danger text-on-accent"
					{busy}
					onclick={confirm}
				>
					{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
					{confirmLabel}
				</Button>
			</div>
		</AlertDialog.Content>
	</AlertDialog.Portal>
</AlertDialog.Root>
