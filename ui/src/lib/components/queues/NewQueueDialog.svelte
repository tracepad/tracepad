<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { Dialog } from 'bits-ui';
	import { goto } from '$app/navigation';
	import { ApiError, api, type ScoreConfig } from '$lib/api/client.svelte';
	import { queueProblem } from '$lib/queues';
	import Button from '../Button.svelte';

	// A queue is a name and the scores it asks for (spec 024 #1, #10). The
	// configs are a multi-select over what the project has declared, because a
	// queue may only name a config that exists — the server refuses the rest,
	// and offering a free text field would be offering that refusal.
	//
	// The `PUT` is declarative, so on an existing name it would replace that
	// queue's list without saying so. Asking first is what makes this button
	// *New*: an existing name is a link, not a write.

	let {
		open = false,
		configs,
		onclose
	}: { open?: boolean; configs: ScoreConfig[]; onclose: () => void } = $props();

	let name = $state('');
	let description = $state('');
	let picked = $state.raw<string[]>([]);
	let busy = $state(false);
	let failure = $state<string | null>(null);

	// Opened again is a fresh form; nothing carries over from the last one.
	$effect(() => {
		if (open) {
			name = description = '';
			picked = [];
			failure = null;
		}
	});

	const problem = $derived(queueProblem(name, picked));

	function toggle(config: string) {
		// The order is the order they were picked, because it is the order the
		// desk will ask the reviewer for them (#1).
		picked = picked.includes(config)
			? picked.filter((one) => one !== config)
			: [...picked, config];
	}

	async function create() {
		busy = true;
		failure = null;
		try {
			const already = await api
				.getQueue(name.trim())
				.then(() => true)
				.catch((cause: unknown) => {
					if (cause instanceof ApiError && cause.status === 404) return false;
					throw cause;
				});
			if (already) {
				failure = `This project already has a queue called ${name.trim()}.`;
				return;
			}
			const queue = await api.putQueue(name.trim(), {
				...(description.trim() === '' ? {} : { description: description.trim() }),
				score_configs: picked
			});
			onclose();
			await goto(`/queues/${encodeURIComponent(queue.name)}`);
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'Failed to create the queue.';
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
			<Dialog.Title class="text-lg font-semibold">New queue</Dialog.Title>
			<Dialog.Description class="text-muted mt-1 text-sm">
				A named list of traces to review, and the scores a reviewer must set on every one of
				them. It starts empty; fill it from a trace, or from the traces a filter matches.
			</Dialog.Description>

			<label for="queue-name" class="mt-3 mb-1 block text-xs font-medium">Name</label>
			<input
				id="queue-name"
				name="name"
				type="text"
				bind:value={name}
				placeholder="weekly-review"
				autocomplete="off"
				spellcheck="false"
				class="{fieldClass} font-mono"
			/>

			<label for="queue-description" class="mt-3 mb-1 block text-xs font-medium">
				Description <span class="text-subtle font-normal">optional</span>
			</label>
			<input
				id="queue-description"
				name="description"
				type="text"
				bind:value={description}
				placeholder="What a reviewer is deciding"
				autocomplete="off"
				class={fieldClass}
			/>

			<fieldset class="mt-3">
				<legend class="text-xs font-medium">Scores to fill</legend>
				{#if configs.length === 0}
					<p class="text-warn mt-1 text-sm">
						This project has declared no score configs. A queue is the scores it asks for, so
						declare one first on <a class="underline" href="/score-configs">Score configs</a>.
					</p>
				{:else}
					<div class="mt-1.5 flex flex-wrap gap-x-4 gap-y-1">
						{#each configs as config (config.name)}
							<label class="flex items-center gap-1.5 text-sm">
								<input
									type="checkbox"
									name="score_configs"
									value={config.name}
									checked={picked.includes(config.name)}
									onchange={() => toggle(config.name)}
								/>
								{config.name}
								<span class="text-subtle text-xs">{config.data_type}</span>
							</label>
						{/each}
					</div>
				{/if}
			</fieldset>

			{#if failure}
				<p role="alert" class="text-danger mt-3 flex items-start gap-2 text-sm">
					<TriangleAlert class="mt-0.5 size-4 shrink-0" />
					{failure}
				</p>
			{:else if problem && (name.trim() !== '' || picked.length > 0)}
				<p class="text-warn mt-3 text-sm">{problem}</p>
			{/if}

			<div class="mt-4 flex justify-end gap-1.5">
				<Dialog.Close>
					{#snippet child({ props })}
						<Button {...props} disabled={busy}>Cancel</Button>
					{/snippet}
				</Dialog.Close>
				<Button variant="primary" disabled={problem !== null} {busy} onclick={create}>
					{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
					Create
				</Button>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
