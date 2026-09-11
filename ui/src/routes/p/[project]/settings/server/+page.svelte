<script lang="ts">
	import AccountsCard from '$lib/components/settings/AccountsCard.svelte';
	import ProjectsCard from '$lib/components/settings/ProjectsCard.svelte';

	// The owner's tab (spec 028 #14): the projects on this server, and the
	// people who can reach them. Everything the admin token used to unlock in a
	// browser, on the credential the browser now holds.
</script>

<ProjectsCard />
<AccountsCard />
