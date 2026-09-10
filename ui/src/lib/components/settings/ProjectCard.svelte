<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import { untrack } from 'svelte';
	import { said } from '$lib/accounts';
	import { api, type Project } from '$lib/api/client.svelte';
	import { timestamp } from '$lib/format';
	import Button from '../Button.svelte';
	import CopyButton from '../CopyButton.svelte';
	import Card from './Card.svelte';

	// The project on screen: what it is called, and what it is called in a
	// request.
	//
	// Renaming is an owner's (spec 028 #3). It was the admin token's for the
	// same reason spec 005 #11 gave — a project's name is the echo every
	// destructive confirmation is typed against — and the field says so to
	// everybody who is not one, rather than simply not being there.

	let {
		current,
		/** Only an owner may rename; a member reads the name (Decision 3). */
		mayRename,
		onchanged
	}: { current: Project; mayRename: boolean; onchanged: () => Promise<void> } = $props();

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
			// The name rides in `me.projects`, so the sidebar and the account
			// menu read the new one from the same call the role comes from.
			await onchanged();
			done = true;
		} catch (cause) {
			failure = said(cause, 'The rename failed.');
		} finally {
			busy = false;
		}
	}
</script>

<Card title="Project" description="The project these screens are showing.">
	<div class="flex flex-wrap items-end gap-2">
		<div class="min-w-0 flex-1">
			<label for="project-name" class="text-muted mb-1 block text-xs font-medium">Name</label>
			<input
				id="project-name"
				type="text"
				bind:value={name}
				disabled={!mayRename}
				autocomplete="off"
				spellcheck="false"
				class="border-border bg-canvas w-full rounded-md border px-2 py-1 text-sm
					disabled:opacity-60"
			/>
		</div>
		{#if mayRename}
			<Button variant="primary" onclick={rename} disabled={!changed} {busy}>
				{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
				Rename
			</Button>
		{/if}
	</div>

	{#if !mayRename}
		<p class="text-subtle mt-1.5 text-xs">
			Renaming a project is an owner's: its name is what every destructive confirmation is typed
			back against.
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
