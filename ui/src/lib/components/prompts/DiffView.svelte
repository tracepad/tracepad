<script lang="ts">
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { ApiError, api } from '$lib/api/client.svelte';
	import { paintDiff, type DiffLine } from '$lib/prompts';

	// The diff between two versions (spec 021 #3). The server computes it —
	// spec 003 put it there so that every client shows the same one — and this
	// paints the lines it sent. Painting a line by what a unified patch makes
	// it is rendering; a diff library here would be a second answer and a
	// dependency spec 006 #4 did not budget.

	let { name, from, to }: { name: string; from: number; to: number } = $props();

	let lines = $state.raw<DiffLine[] | null>(null);
	let failure = $state<string | null>(null);

	$effect(() => {
		const controller = new AbortController();
		void load(name, from, to, controller.signal);
		return () => controller.abort();
	});

	async function load(named: string, a: number, b: number, signal: AbortSignal) {
		failure = null;
		lines = null;
		try {
			const answer = await api.promptDiff(named, a, b, signal);
			if (!signal.aborted) lines = paintDiff(answer.diff);
		} catch (cause) {
			if (signal.aborted) return;
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the diff.';
		}
	}

	/** Per kind, in tokens that hold their contrast in both themes (spec 015 #6). */
	const paint: Record<DiffLine['kind'], string> = {
		add: 'bg-ok-soft text-ok',
		remove: 'bg-danger-soft text-danger',
		hunk: 'text-accent',
		file: 'text-subtle',
		context: 'text-muted'
	};
</script>

{#if failure}
	<p role="alert" class="text-danger flex items-start gap-2 p-4 text-sm">
		<TriangleAlert class="mt-0.5 size-4 shrink-0" />
		{failure}
	</p>
{:else if lines === null}
	<p class="text-subtle flex items-center gap-2 p-4 text-sm">
		<LoaderCircle class="size-4 animate-spin" />
		Reading the diff
	</p>
{:else if lines.length === 0}
	<p class="text-muted p-4 text-sm">
		v{from} and v{to} are identical — the same body and the same config.
	</p>
{:else}
	<div class="min-w-0 overflow-x-auto p-4">
		<pre
			class="font-mono text-xs leading-5"
			aria-label="Diff of v{from} and v{to}">{#each lines as line, i (i)}<div
					class="{paint[line.kind]} w-max min-w-full px-2"
					data-kind={line.kind}>{line.text || ' '}</div>{/each}</pre>
	</div>
{/if}
