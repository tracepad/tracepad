<script lang="ts">
	import { modified } from '$lib/peek';
	import { href } from '$lib/project.svelte';
	import { active, isGroup, type Group, type Item } from '$lib/sections';

	// The sections as a list of links, a group's label a heading over its
	// children and the screen on show lit: the desktop column's list, and the
	// phone's *More* sheet (spec 006 #20), whose rows are a thumb tall whatever
	// the pointer says.
	let {
		sections,
		touch = false,
		onnavigate
	}: {
		sections: (Item | Group)[];
		touch?: boolean;
		/**
		 * A link followed here, the sheet closes on the tap rather than on
		 * arrival; one opened elsewhere — a new tab — leaves this page as it is.
		 */
		onnavigate?: () => void;
	} = $props();

	const link = $derived(
		touch
			? 'flex min-h-11 items-center gap-3 rounded-md px-3 text-lg transition-colors duration-100'
			: 'flex items-center gap-2 rounded-md px-2 py-1.5 transition-colors duration-100 pointer-coarse:min-h-11'
	);
	const lit = 'bg-accent-soft text-accent font-medium';
	const dim = 'text-muted hover:bg-raised hover:text-fg';
	const list = $derived(touch ? 'flex flex-col gap-0.5' : undefined);
</script>

{#snippet item(entry: Item, indented: boolean)}
	{@const Glyph = entry.icon}
	{@const on = active(entry.href)}
	<li>
		<a
			href={href(entry.href)}
			aria-current={on ? 'page' : undefined}
			onclick={(event) => !modified(event) && onnavigate?.()}
			class={[link, indented && !touch && 'ml-3', on ? lit : dim]}
		>
			<Glyph class={touch ? 'size-4.5 shrink-0' : 'size-4 shrink-0'} />
			{entry.label}
		</a>
	</li>
{/snippet}

<ul class={list}>
	{#each sections as section (section.label)}
		{#if isGroup(section)}
			<li class={touch ? 'mt-2' : undefined}>
				<span
					class={[
						'text-subtle block text-xs font-medium tracking-wide uppercase',
						touch ? 'px-3 py-1.5' : 'mt-2 px-2 py-1'
					]}
				>
					{section.label}
				</span>
				<ul class={list}>
					{#each section.children as child (child.href)}
						{@render item(child, true)}
					{/each}
				</ul>
			</li>
		{:else}
			{@render item(section, false)}
		{/if}
	{/each}
</ul>
