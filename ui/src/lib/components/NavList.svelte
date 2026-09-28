<script lang="ts">
	import { href } from '$lib/project.svelte';
	import { active, isGroup, type Group, type Item } from '$lib/sections';

	// The sections as a column of touch rows, for the phone's *More* sheet
	// (spec 006 #20): the same data as the desktop column, a row tall enough
	// for a thumb whatever the pointer says, the group label a heading, and
	// the screen on show lit as it is there.
	let { sections }: { sections: (Item | Group)[] } = $props();

	const row = 'flex min-h-11 items-center gap-3 rounded-md px-3 text-lg transition-colors duration-100';
	const lit = 'bg-accent-soft text-accent font-medium';
	const dim = 'text-muted hover:bg-raised hover:text-fg';
</script>

{#snippet item(entry: Item)}
	{@const Glyph = entry.icon}
	<li>
		<a
			href={href(entry.href)}
			aria-current={active(entry.href) ? 'page' : undefined}
			class={[row, active(entry.href) ? lit : dim]}
		>
			<Glyph class="size-4.5 shrink-0" />
			{entry.label}
		</a>
	</li>
{/snippet}

<ul class="flex flex-col gap-0.5">
	{#each sections as section (section.label)}
		{#if isGroup(section)}
			<li class="mt-2">
				<span class="text-subtle block px-3 py-1.5 text-xs font-medium tracking-wide uppercase">
					{section.label}
				</span>
				<ul class="flex flex-col gap-0.5">
					{#each section.children as child (child.href)}
						{@render item(child)}
					{/each}
				</ul>
			</li>
		{:else}
			{@render item(section)}
		{/if}
	{/each}
</ul>
