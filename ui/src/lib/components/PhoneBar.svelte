<script lang="ts">
	import { switcher } from '$lib/project.svelte';
	import AccountMenu from './AccountMenu.svelte';
	import BrandMark from './BrandMark.svelte';
	import ProjectSwitcher from './ProjectSwitcher.svelte';

	// A phone's bar on top (spec 006 #20): the product, the project and the
	// account, 48 px, and nothing that navigates — the sections are tabs under
	// the page (`PhoneTabs`).
</script>

<header class="border-border bg-surface flex h-12 shrink-0 items-center gap-2 border-b pr-1 pl-3">
	<BrandMark class="h-4" />
	<span class="shrink-0 text-lg font-semibold tracking-tight">Tracepad</span>
	<div class="flex min-w-0 flex-1 items-center">
		<ProjectSwitcher bind:open={switcher.open} />
	</div>
	<AccountMenu side="bottom" />
</header>
