<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import { untrack } from 'svelte';
	import { admin } from '$lib/admin.svelte';
	import { ApiError, api, type Project } from '$lib/api/client.svelte';
	import { project } from '$lib/project.svelte';
	import { timestamp } from '$lib/format';
	import Button from '../Button.svelte';
	import CopyButton from '../CopyButton.svelte';
	import Card from './Card.svelte';

	// The project this key belongs to: what it is called, and what it is called
	// in a request.
	//
	// Renaming is the one thing on this card that the session key cannot do.
	// Spec 005 #11 made a rename cross-project administration — it is how a
	// project is identified in every confirmation echo — so it runs on the
	// admin token, and the field says so until the Administration section
	// below is unlocked (spec 007 #12).

	let { current }: { current: Project } = $props();

	// Seeded from the stored row and then owned by whoever is typing: a
	// refresh after a successful rename must not reach into the field and
	// rewrite it under them. `untrack` says that is deliberate.
	let name = $state(untrack(() => current.name));
	let busy = $state(false);
	let failure = $state<string | null>(null);
	let done = $state(false);

	const changed = $derived(name.trim() !== '' && name.trim() !== current.name);

	async function rename() {
		busy = true;
		failure = null;
		done = false;
		try {
			await api.renameProject(current.id, name.trim());
			await project.refresh();
			done = true;
		} catch (cause) {
			// A 403 here is the server explaining its own credential split;
			// it says it better than a paraphrase would (spec 007 #4).
			failure = cause instanceof ApiError ? cause.message : 'The rename failed.';
		} finally {
			busy = false;
		}
	}
</script>

<Card title="Project" description="The project this key reads and writes.">
	<div class="flex flex-wrap items-end gap-2">
		<div class="min-w-0 flex-1">
			<label for="project-name" class="text-muted mb-1 block text-xs font-medium">Name</label>
			<input
				id="project-name"
				type="text"
				bind:value={name}
				disabled={!admin.unlocked}
				autocomplete="off"
				spellcheck="false"
				class="border-border bg-canvas w-full rounded-md border px-2 py-1 text-sm
					disabled:opacity-60"
			/>
		</div>
		<Button variant="primary" onclick={rename} disabled={!changed || !admin.unlocked} busy={busy}>
			{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
			Rename
		</Button>
	</div>

	{#if !admin.unlocked}
		<p class="text-subtle mt-1.5 text-xs">
			Renaming needs the admin token — a project's name is what every destructive confirmation
			echoes. Unlock Administration below, or use <code class="font-mono"
				>tracepad projects rename</code
			>.
		</p>
	{/if}
	{#if failure}
		<p role="alert" class="text-danger mt-2 text-sm">{failure}</p>
	{:else if done}
		<p role="status" class="text-ok mt-2 text-sm">Renamed.</p>
	{/if}

	<dl class="text-muted mt-4 flex flex-wrap gap-x-8 gap-y-2 text-sm">
		<div class="flex min-w-0 items-baseline gap-1.5">
			<dt class="text-subtle text-xs">Project id</dt>
			<dd class="truncate font-mono text-xs">{current.id}</dd>
			<CopyButton text={current.id} label="Copy the project id" />
		</div>
		<div class="flex items-baseline gap-1.5">
			<dt class="text-subtle text-xs">Created</dt>
			<dd class="tabular-nums">{timestamp(current.created_at)}</dd>
		</div>
	</dl>
</Card>
