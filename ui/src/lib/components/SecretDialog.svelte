<script lang="ts">
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import X from '@lucide/svelte/icons/x';
	import { Dialog } from 'bits-ui';
	import { page } from '$app/state';
	import type { NewKey } from '$lib/api/client.svelte';
	import Button from './Button.svelte';
	import CopyButton from './CopyButton.svelte';

	// A minted key pair, shown once (spec 007 #9). The secret is stored hashed
	// (spec 005 #12) and can never be read back, so this dialog says so plainly
	// instead of pretending the key is somewhere to be found later.
	//
	// The lines follow the key's scopes (spec 045 #14): every key gets the
	// variables the Tracepad packages, the CLI and the MCP server read
	// (`TRACEPAD_URL`, one name for all three, spec 017 #21), since the
	// packages export with it too; and
	// only a key that can ingest gets the exporter formats — a `read` key
	// pasted into an exporter's headers is a mistake this can prevent by not
	// suggesting it. A pair without scopes is a new project's first, which
	// holds all three (#5).

	let { pair, onclose }: { pair: NewKey | null; onclose: () => void } = $props();

	const origin = $derived(page.url.origin);
	const formats = $derived.by(() => {
		if (!pair) return [];
		const tracepad = {
			label: 'Tracepad packages, CLI and MCP',
			body:
				`TRACEPAD_URL=${origin}\n` +
				`TRACEPAD_API_KEY=${pair.secret_key}`
		};
		if (pair.scopes && !pair.scopes.includes('ingest')) return [tracepad];
		return [
			tracepad,
			{
				label: 'OpenTelemetry SDK',
				body:
					`OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf\n` +
					`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=${origin}/v1/traces\n` +
					`OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer ${pair.secret_key}"`
			},
			{
				label: 'Langfuse SDK',
				body:
					`LANGFUSE_BASE_URL=${origin}\n` +
					`# the older name, read by older SDKs\n` +
					`LANGFUSE_HOST=${origin}\n` +
					`LANGFUSE_PUBLIC_KEY=${pair.public_key}\n` +
					`LANGFUSE_SECRET_KEY=${pair.secret_key}`
			}
		];
	});
</script>

<Dialog.Root open={pair !== null} onOpenChange={(open) => !open && onclose()}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content
			class="border-border bg-canvas shadow-overlay fixed top-1/2 left-1/2 z-50 max-h-[90dvh]
				w-[min(36rem,calc(100vw-1.5rem))] -translate-x-1/2 -translate-y-1/2 overflow-y-auto
				rounded-lg border p-4"
		>
			<div class="flex items-start gap-3">
				<div class="min-w-0 flex-1">
					<Dialog.Title class="text-lg font-semibold">Your new key pair</Dialog.Title>
					<Dialog.Description class="text-muted mt-1">
						Copy it now. The secret is stored only as a hash — nobody, including this page, can
						show it again.
					</Dialog.Description>
				</div>
				<Dialog.Close>
					{#snippet child({ props })}
						<button
							{...props}
							aria-label="Close"
							class="text-muted hover:bg-raised hover:text-fg pointer-coarse:size-11 inline-flex
								size-7 shrink-0 cursor-pointer items-center justify-center rounded-md
								transition-colors duration-100"
						>
							<X class="size-4" />
						</button>
					{/snippet}
				</Dialog.Close>
			</div>

			<p class="text-warn mt-3 flex items-start gap-2 text-sm">
				<TriangleAlert class="mt-0.5 size-4 shrink-0" />
				This is the only time the secret key is shown.
			</p>

			{#each formats as format (format.label)}
				<h3 class="text-subtle mt-4 text-xs font-medium">{format.label}</h3>
				<div class="border-border bg-surface mt-1 flex items-start gap-2 rounded-md border p-3">
					<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{format.body}</pre>
					<CopyButton text={format.body} label="Copy the {format.label} settings" />
				</div>
			{/each}

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
