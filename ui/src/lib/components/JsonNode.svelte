<script lang="ts" module>
	/** How much of a string is shown before the rest has to be asked for. */
	export const STRING_PREVIEW = 180;

	export function isBranch(value: unknown): value is object {
		return typeof value === 'object' && value !== null;
	}

	/** What a closed branch says about itself. */
	export function summarise(value: object): string {
		const size = Array.isArray(value) ? value.length : Object.keys(value).length;
		const unit = Array.isArray(value)
			? size === 1
				? 'item'
				: 'items'
			: size === 1
				? 'key'
				: 'keys';
		return Array.isArray(value) ? `[ ${size} ${unit} ]` : `{ ${size} ${unit} }`;
	}
</script>

<script lang="ts">
	import { untrack } from 'svelte';
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';
	import Self from './JsonNode.svelte';
	import CopyButton from './CopyButton.svelte';

	// The hand-written lazy JSON tree (design §8). Lazy in the sense that
	// matters for a two-megabyte payload: a closed branch renders nothing at
	// all, and a long string renders its first lines until asked for the rest.
	//
	// It knows nothing about budgets or truncation markers — that is the
	// payload wrapper's job (spec 004 #2). Here it is only JSON.

	let { value, name, depth = 0 }: { value: unknown; name?: string; depth?: number } = $props();

	// The first two levels are the shape of the thing; deeper is detail worth
	// asking for. A node's depth never changes under it, so this reads the
	// prop once and then belongs to whoever is clicking.
	let open = $state(untrack(() => depth) < 2);
	let whole = $state(false);

	const branch = $derived(isBranch(value) ? value : null);
	const entries = $derived.by(() => {
		if (!branch) return [] as (readonly [string, unknown])[];
		return Array.isArray(branch)
			? branch.map((item, index) => [String(index), item] as const)
			: Object.entries(branch as Record<string, unknown>);
	});

	const text = $derived(typeof value === 'string' ? value : '');
	const long = $derived(text.length > STRING_PREVIEW);
</script>

<div class="group/node">
	<div class="flex items-start gap-1">
		{#if branch}
			<button
				type="button"
				onclick={() => (open = !open)}
				aria-expanded={open}
				class="hover:text-fg flex min-w-0 cursor-pointer items-start gap-1 text-left"
			>
				{#if open}
					<ChevronDown class="text-subtle mt-0.5 size-3.5 shrink-0" />
				{:else}
					<ChevronRight class="text-subtle mt-0.5 size-3.5 shrink-0" />
				{/if}
				{#if name !== undefined}<span class="text-fg truncate">{name}</span>{/if}
				<span class="text-subtle whitespace-nowrap">
					{open ? (Array.isArray(branch) ? '[' : '{') : summarise(branch)}
				</span>
			</button>
		{:else}
			<span class="w-3.5 shrink-0"></span>
			{#if name !== undefined}<span class="text-fg shrink-0">{name}:</span>{/if}
			<span class="min-w-0 break-all">
				{#if typeof value === 'string'}
					<span class="text-code-string">"{long && !whole ? text.slice(0, STRING_PREVIEW) : text}"</span
					>{#if long}<button
							type="button"
							onclick={() => (whole = !whole)}
							class="text-accent ml-1 cursor-pointer underline underline-offset-2"
						>
							{whole ? 'less' : `${text.length - STRING_PREVIEW} more characters`}
						</button>{/if}
				{:else if typeof value === 'number'}
					<span class="text-code-number tabular-nums">{value}</span>
				{:else}
					<span class="text-muted">{value === null ? 'null' : String(value)}</span>
				{/if}
			</span>
		{/if}
		<span
			class="opacity-0 transition-opacity duration-100 group-hover/node:opacity-100 focus-within:opacity-100"
		>
			<CopyButton text={() => JSON.stringify(value, null, 2)} label="Copy this value" />
		</span>
	</div>

	{#if branch && open}
		<!-- Rendered only while open: a closed branch costs nothing. -->
		<div class="border-border ml-[0.4375rem] border-l pl-2">
			{#each entries as [key, child] (key)}
				<Self value={child} name={key} depth={depth + 1} />
			{/each}
		</div>
	{/if}
</div>
