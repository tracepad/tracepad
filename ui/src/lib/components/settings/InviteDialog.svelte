<script lang="ts">
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { Dialog } from 'bits-ui';
	import { timestamp } from '$lib/format';
	import Button from '../Button.svelte';
	import CopyButton from '../CopyButton.svelte';

	// The invitation link, shown once (spec 028 #10). The server hands it to the
	// owner and carrying it to the person is the owner's job — there is no mail
	// here — so this dialog is the one place it ever appears, and it says so
	// rather than pretending it can be found again.
	//
	// It is `SecretDialog`'s shape for the same reason: a thing you copy now.

	let {
		link,
		expires,
		note,
		onclose
	}: { link: string | null; expires: string; note?: string; onclose: () => void } = $props();
</script>

<Dialog.Root open={link !== null} onOpenChange={(open) => !open && onclose()}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content
			class="border-border bg-canvas shadow-overlay fixed top-1/2 left-1/2 z-50 max-h-[90dvh]
				w-[min(36rem,calc(100vw-1.5rem))] -translate-x-1/2 -translate-y-1/2 overflow-y-auto
				rounded-lg border p-4"
		>
			<Dialog.Title class="text-lg font-semibold">The invitation link</Dialog.Title>
			<Dialog.Description class="text-muted mt-1">
				Send it to them yourself — this server sends no mail. It sets their password, works once,
				and expires {timestamp(expires)}.
			</Dialog.Description>

			<p class="text-warn mt-3 flex items-start gap-2 text-sm">
				<TriangleAlert class="mt-0.5 size-4 shrink-0" />
				This is the only time the link is shown. A fresh one voids this.
			</p>

			<div class="border-border bg-surface mt-3 flex items-start gap-2 rounded-md border p-3">
				<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{link ?? ''}</pre>
				<CopyButton text={link ?? ''} label="Copy the invitation link" />
			</div>

			{#if note}
				<p class="text-muted mt-3 text-sm">{note}</p>
			{/if}

			<div class="mt-4 flex justify-end">
				<Dialog.Close>
					{#snippet child({ props })}
						<Button {...props} variant="primary">I have copied it</Button>
					{/snippet}
				</Dialog.Close>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
