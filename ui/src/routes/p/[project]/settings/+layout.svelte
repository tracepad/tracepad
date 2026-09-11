<script lang="ts">
	import SettingsTabs from '$lib/components/settings/SettingsTabs.svelte';

	// The two tabs about a project (spec 028 #14), under its prefix (spec 029
	// #1); the strip they sit in is shared with the bare Account tab (#14).

	let { children } = $props();
</script>

<SettingsTabs>{@render children()}</SettingsTabs>
