<script lang="ts">
	import { api } from '$lib/api/client.svelte';
	import { switcher } from '$lib/project.svelte';
	import { SECTIONS } from '$lib/sections';
	import AccountMenu from './AccountMenu.svelte';
	import NavList from './NavList.svelte';
	import ProjectSwitcher from './ProjectSwitcher.svelte';
	import ThemeToggle from './ThemeToggle.svelte';

	// The desktop's column. A phone has the same destinations in a bar on top
	// and tabs under the page (`PhoneBar`, `PhoneTabs`, spec 006 #20), and the
	// shell renders one shape or the other, never both.
</script>

<aside class="border-border bg-surface flex h-full w-52 shrink-0 flex-col border-r">
	<div class="flex h-12 shrink-0 items-center px-3">
		<span class="text-lg font-semibold tracking-tight">Tracepad</span>
	</div>

	<!-- The switcher (spec 029 #5), in the space the project name had. -->
	<div class="flex min-w-0 items-center px-2 pb-2">
		<ProjectSwitcher bind:open={switcher.open} />
	</div>

	<nav class="min-w-0 flex-1 px-2" aria-label="Sections">
		<NavList sections={SECTIONS} />
	</nav>

	<!-- Who is signed in sits at the bottom of the column (spec 028 #14): the
	     last thing on the way out, and the one control that is about the
	     reader rather than the data. -->
	<div class="border-border flex shrink-0 flex-col gap-1 border-t p-2">
		<div class="flex items-center justify-between gap-0.5">
			<span class="text-subtle px-1 font-mono text-xs" title="Server version">
				{api.version ?? ''}
			</span>
			<ThemeToggle />
		</div>
		<AccountMenu />
	</div>
</aside>
