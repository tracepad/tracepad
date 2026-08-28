<script lang="ts">
	import { ListTree, LogOut } from '@lucide/svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api } from '$lib/api/client.svelte';
	import { auth, LOGIN_ROUTE } from '$lib/auth.svelte';
	import { project } from '$lib/project.svelte';
	import ThemeToggle from './ThemeToggle.svelte';

	// Navigation as data, so spec 007 adds a screen by adding a row.
	const SECTIONS = [{ href: '/traces', label: 'Traces', icon: ListTree }];

	// One markup, two shapes (spec 006 #15): a column down the left on a
	// desktop, a single bar across the top on a phone. Utilities rather than a
	// second component — the parts and their order are the same either way.

	function signOut() {
		auth.clear();
		project.forget();
		goto(LOGIN_ROUTE, { replaceState: true });
	}
</script>

<aside
	class="border-border bg-surface flex shrink-0 items-center gap-2 border-b px-3
		md:h-full md:w-52 md:flex-col md:items-stretch md:gap-0 md:border-r md:border-b-0 md:px-0"
>
	<div class="flex h-12 shrink-0 items-center md:px-3">
		<span class="text-lg font-semibold tracking-tight">Tracepad</span>
	</div>

	<div
		class="text-subtle min-w-0 shrink truncate text-xs md:shrink-0 md:px-3 md:pb-2"
		title={project.name ?? undefined}
	>
		{project.name ?? '—'}
	</div>

	<nav class="min-w-0 flex-1 md:px-2" aria-label="Sections">
		<ul class="flex gap-1 md:block">
			{#each SECTIONS as section (section.href)}
				{@const active = page.url.pathname.startsWith(section.href)}
				{@const Glyph = section.icon}
				<li>
					<a
						href={section.href}
						aria-current={active ? 'page' : undefined}
						class={[
							'flex items-center gap-2 rounded-md px-2 py-1.5 transition-colors duration-100',
							'pointer-coarse:min-h-11',
							active
								? 'bg-accent-soft text-accent font-medium'
								: 'text-muted hover:bg-raised hover:text-fg'
						]}
					>
						<Glyph class="size-4 shrink-0" />
						{section.label}
					</a>
				</li>
			{/each}
		</ul>
	</nav>

	<div
		class="border-border flex shrink-0 items-center gap-0.5
			md:justify-between md:border-t md:px-2 md:py-2"
	>
		<span class="text-subtle hidden px-1 font-mono text-xs md:inline" title="Server version">
			{api.version ?? ''}
		</span>
		<div class="flex items-center gap-0.5">
			<ThemeToggle />
			<button
				type="button"
				onclick={signOut}
				title="Sign out"
				aria-label="Sign out"
				class="text-muted hover:bg-raised hover:text-fg pointer-coarse:size-11 inline-flex size-7
					cursor-pointer items-center justify-center rounded-md transition-colors duration-100"
			>
				<LogOut class="size-4" />
			</button>
		</div>
	</div>
</aside>
