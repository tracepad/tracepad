<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { Dialog } from 'bits-ui';
	import { goto } from '$app/navigation';
	import { ApiError, api } from '$lib/api/client.svelte';
	import { href } from '$lib/project.svelte';
	import Button from '../Button.svelte';

	// A dataset is a name and a sentence (spec 014's envelope): the `PUT` that
	// makes one takes nothing else, and the cases arrive afterwards — pushed by
	// the CLI or written in the editor. Declarative, so the same call on an
	// existing name would replace its description; the form refuses a name the
	// project already has rather than quietly rewriting it.

	let { open = false, onclose }: { open?: boolean; onclose: () => void } = $props();

	let name = $state('');
	let description = $state('');
	let busy = $state(false);
	let failure = $state<string | null>(null);

	// Opened again is a fresh form; nothing carries over from the last one.
	$effect(() => {
		if (open) {
			name = description = '';
			failure = null;
		}
	});

	const ready = $derived(name.trim() !== '');

	async function create() {
		busy = true;
		failure = null;
		try {
			// The `PUT` is declarative, so on an existing name it would replace
			// that dataset's description without saying so. Asking first is what
			// makes this button *New*: an existing name is a link, not a write.
			const already = await api
				.getDataset(name.trim())
				.then(() => true)
				.catch((cause: unknown) => {
					if (cause instanceof ApiError && cause.status === 404) return false;
					throw cause;
				});
			if (already) {
				failure = `This project already has a dataset called ${name.trim()}.`;
				return;
			}
			const dataset = await api.putDataset(name.trim(), {
				...(description.trim() === '' ? {} : { description: description.trim() })
			});
			onclose();
			await goto(href(`/datasets/${encodeURIComponent(dataset.name)}`));
		} catch (cause) {
			// The name grammar is the server's (a URL path segment), and so is
			// the sentence that explains it.
			failure = cause instanceof ApiError ? cause.message : 'Failed to create the dataset.';
		} finally {
			busy = false;
		}
	}

	const fieldClass = 'border-border bg-canvas text-fg w-full rounded-md border px-2 py-1 text-sm';
</script>

<Dialog.Root {open} onOpenChange={(next) => !next && !busy && onclose()}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content
			class="border-border bg-canvas shadow-overlay fixed top-1/2 left-1/2 z-50 max-h-[90dvh]
				w-[min(28rem,calc(100vw-1.5rem))] -translate-x-1/2 -translate-y-1/2 overflow-y-auto
				rounded-lg border p-4"
		>
			<Dialog.Title class="text-lg font-semibold">New dataset</Dialog.Title>
			<Dialog.Description class="text-muted mt-1 text-sm">
				A named, versioned set of test cases. It starts empty at version 0; push cases with the
				CLI or add them one at a time in the editor.
			</Dialog.Description>

			<label for="dataset-name" class="mt-3 mb-1 block text-xs font-medium">Name</label>
			<input
				id="dataset-name"
				name="name"
				type="text"
				bind:value={name}
				placeholder="support-golden"
				autocomplete="off"
				spellcheck="false"
				class="{fieldClass} font-mono"
			/>

			<label for="dataset-description" class="mt-3 mb-1 block text-xs font-medium">
				Description <span class="text-subtle font-normal">optional</span>
			</label>
			<input
				id="dataset-description"
				name="description"
				type="text"
				bind:value={description}
				placeholder="Answers support should get right"
				autocomplete="off"
				class={fieldClass}
			/>

			{#if failure}
				<p role="alert" class="text-danger mt-3 flex items-start gap-2 text-sm">
					<TriangleAlert class="mt-0.5 size-4 shrink-0" />
					{failure}
				</p>
			{/if}

			<div class="mt-4 flex justify-end gap-1.5">
				<Dialog.Close>
					{#snippet child({ props })}
						<Button {...props} disabled={busy}>Cancel</Button>
					{/snippet}
				</Dialog.Close>
				<Button variant="primary" disabled={!ready} {busy} onclick={create}>
					{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
					Create
				</Button>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
