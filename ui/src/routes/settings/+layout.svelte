<script lang="ts">
	import SettingsTabs from '$lib/components/settings/SettingsTabs.svelte';

	// The one tab about the person rather than a project, so the one screen
	// inside the shell that lives outside the prefix (spec 029 #14): a name
	// and a password belong to an account whether or not it reaches a
	// project. Same strip as under `/p/{id}/settings`, same sidebar.

	let { children } = $props();
</script>

<SettingsTabs>{@render children()}</SettingsTabs>
