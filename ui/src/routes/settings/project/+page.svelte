<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import { said } from '$lib/accounts';
	import { api, type Project } from '$lib/api/client.svelte';
	import { project } from '$lib/project.svelte';
	import EraseCard from '$lib/components/settings/EraseCard.svelte';
	import KeysCard from '$lib/components/settings/KeysCard.svelte';
	import ProjectCard from '$lib/components/settings/ProjectCard.svelte';
	import RetentionCard from '$lib/components/settings/RetentionCard.svelte';
	import { refresh } from '$lib/session';

	// The project on screen, as its members may change it (spec 028 #14). Every
	// card is here for every role: a viewer sees what the project is set to and
	// one line saying why the buttons are gone, which is spec 007 #12's rule
	// applied to roles rather than to a credential.

	const id = $derived(project.id);
	const viewer = $derived(project.role === 'viewer');
	const owner = $derived(project.role === 'owner');

	let current = $state.raw<Project | null>(null);
	let failure = $state<string | null>(null);

	$effect(() => {
		const controller = new AbortController();
		void load(id, controller.signal);
		return () => controller.abort();
	});

	async function load(wanted: string | null, signal?: AbortSignal) {
		if (!wanted) return;
		failure = null;
		try {
			const answer = await api.getProject(wanted, signal);
			if (!signal?.aborted) current = answer;
		} catch (cause) {
			if (signal?.aborted) return;
			failure = said(cause, 'Failed to read the project.');
		}
	}

	/**
	 * What a card calls when it changed the row. The project's name rides in
	 * `me.projects`, so a rename has to reach the session as well as this
	 * screen — one call each, in that order, so the sidebar and the cards never
	 * disagree about what the project is called (Decision 15).
	 */
	async function changed() {
		await refresh();
		await load(id);
	}
</script>

{#if failure}
	<p role="alert" class="text-danger text-sm">{failure}</p>
{:else if current}
	<ProjectCard {current} mayRename={owner} onchanged={changed} />
	<RetentionCard {current} readOnly={viewer} onchanged={changed} />
	<KeysCard {current} readOnly={viewer} />
	<EraseCard {current} readOnly={viewer} />
{:else}
	<p class="text-subtle flex items-center gap-2 text-sm">
		<LoaderCircle class="size-4 animate-spin" />
		Reading the project
	</p>
{/if}
