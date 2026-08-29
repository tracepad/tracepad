<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { project } from '$lib/project.svelte';
	import PageHeader from '$lib/components/PageHeader.svelte';
	import AdminSection from '$lib/components/settings/AdminSection.svelte';
	import EraseCard from '$lib/components/settings/EraseCard.svelte';
	import KeysCard from '$lib/components/settings/KeysCard.svelte';
	import ProjectCard from '$lib/components/settings/ProjectCard.svelte';
	import RetentionCard from '$lib/components/settings/RetentionCard.svelte';

	// Settings runs on two credentials with disjoint powers (spec 007 #3): the
	// session's project key manages its own project — name, retention, keys,
	// erasure — and the Administration section at the bottom unlocks with the
	// admin token, which manages project lifecycle and never touches trace
	// data.
	//
	// The screen composes the two rather than asking the server to blur them:
	// the split is spec 005 #11's, and it stays exactly where it was.

	const current = $derived(project.current);
</script>

<svelte:head><title>Settings · Tracepad</title></svelte:head>

<PageHeader title="Settings" />

<div class="min-h-0 flex-1 overflow-auto p-4">
	<div class="mx-auto flex max-w-3xl flex-col gap-4">
		{#if current}
			<ProjectCard {current} />
			<RetentionCard {current} />
			<KeysCard {current} />
			<EraseCard {current} />
		{:else}
			<p class="text-subtle flex items-center gap-2 text-sm">
				<LoaderCircle class="size-4 animate-spin" />
				Reading the project
			</p>
			<noscript>
				<p role="alert" class="text-danger flex items-center gap-2 text-sm">
					<TriangleAlert class="size-4" />
					This interface needs JavaScript.
				</p>
			</noscript>
		{/if}

		<AdminSection />
	</div>
</div>
