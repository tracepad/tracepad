<script lang="ts">
	import Plus from '@lucide/svelte/icons/plus';
	import { goto } from '$app/navigation';
	import type { Project } from '$lib/api/client.svelte';
	import { auth } from '$lib/auth.svelte';
	import Button from '$lib/components/Button.svelte';
	import NewProjectDialog from '$lib/components/settings/NewProjectDialog.svelte';
	import { under } from '$lib/project.svelte';

	// An account that reaches no project (spec 029 #4): a fresh server after
	// setup, every project deleted, or a member whose last membership went.
	// For an owner it is the first step of a new server, so the button is
	// here; for anybody else the next step is somebody else's.

	let creating = $state(false);
	/** The project just made, whose listing this screen leaves for once the keys are put away. */
	let made = $state.raw<Project | null>(null);

	// The keys are shown once, and this screen is what the dialog is mounted
	// in: leaving before they are dismissed would take them with it.
	function closed() {
		creating = false;
		if (made) void goto(under('/traces', made.id));
	}
</script>

<div class="flex flex-1 items-center justify-center p-8">
	<div class="max-w-md text-center">
		<h1 class="text-xl font-semibold">No projects yet</h1>
		{#if auth.owner}
			<p class="text-muted mt-2">
				A project is where traces land. Create one and it hands you the keys an exporter needs.
			</p>
			<Button variant="primary" class="mt-4" onclick={() => (creating = true)}>
				<Plus class="size-4" />
				New project
			</Button>
		{:else}
			<p class="text-muted mt-2">
				This account is not a member of any project. Ask an owner to add you to one.
			</p>
		{/if}
	</div>
</div>

{#if auth.owner}
	<NewProjectDialog open={creating} onclose={closed} oncreated={(created) => (made = created)} />
{/if}
