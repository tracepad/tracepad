<script lang="ts">
	import { type Prompt } from '$lib/api/client.svelte';
	import { timestamp } from '$lib/format';
	import { href } from '$lib/project.svelte';
	import { messagesOf } from '$lib/prompts';
	import JsonView from '../json/JsonView.svelte';
	import LabelControl from './LabelControl.svelte';

	// One version, read (spec 021 #2): the body as it is — role-labelled blocks
	// for a chat prompt, one block for a text one — the config as a document,
	// and the two links that answer "where did this run".
	//
	// The body is *not* a `JsonView`: a prompt is prose, and a person reading a
	// system message wants the paragraph, not the escaped one-line string that
	// paragraph is stored as. `config` is a document, and gets the document
	// surface every other payload in this interface gets.

	let {
		prompt,
		named,
		onchanged
	}: {
		prompt: Prompt;
		/** The name's labels, or `null` until a version listing has answered. */
		named: Record<string, number> | null;
		onchanged: () => void;
	} = $props();

	const messages = $derived(prompt.type === 'chat' ? messagesOf(prompt.prompt) : []);
	const text = $derived(
		typeof prompt.prompt === 'string' ? prompt.prompt : JSON.stringify(prompt.prompt, null, 2)
	);
	/**
	 * The trace listing filtered by this prompt. The whole value is encoded,
	 * `@` included: a name may hold one itself (`team@acme/answer`, spec 012
	 * #15), and the filter's grammar reads the run of digits after the *last*
	 * `@` — so the pair has to arrive as one value, not as two joined in a URL.
	 */
	const filtered = (at?: number) =>
		href(`/traces?prompt=${encodeURIComponent(at === undefined ? prompt.name : `${prompt.name}@${at}`)}`);

	const block = 'border-border bg-surface rounded-md border';
</script>

<div class="flex flex-col gap-3 p-4">
	<div class="flex flex-wrap items-baseline gap-x-3 gap-y-1">
		<h2 class="text-base font-semibold tabular-nums">v{prompt.version}</h2>
		{#if prompt.commit_message}
			<span class="text-muted min-w-0 text-sm">{prompt.commit_message}</span>
		{/if}
		<span class="text-subtle font-mono text-xs tabular-nums">{timestamp(prompt.created_at)}</span>
	</div>

	<LabelControl
		name={prompt.name}
		version={prompt.version}
		labels={prompt.labels}
		{named}
		{onchanged}
	/>

	{#if prompt.type === 'chat'}
		<div class="flex flex-col gap-2">
			{#each messages as message, i (i)}
				<div class={block}>
					<p class="text-subtle border-border border-b px-3 py-1 font-mono text-xs">
						{message.role}
					</p>
					<pre class="overflow-x-auto px-3 py-2 font-mono text-xs whitespace-pre-wrap">{message.content}</pre>
				</div>
			{/each}
		</div>
	{:else}
		<div class={block}>
			<pre class="overflow-x-auto px-3 py-2 font-mono text-xs whitespace-pre-wrap">{text}</pre>
		</div>
	{/if}

	{#if prompt.config}
		<section class="flex flex-col gap-1.5">
			<h3 class="text-muted text-xs font-medium tracking-wide uppercase">Config</h3>
			<JsonView value={prompt.config} label="Config" />
		</section>
	{/if}

	<!-- Where it ran: the filter spec 012 added answers this, so the page links
	     rather than counting (#2) — a count per version is the fan-out spec 016
	     #16 refused. -->
	<p class="text-subtle flex flex-wrap gap-x-3 text-xs">
		<a class="text-accent underline underline-offset-2" href={filtered(prompt.version)}>
			Traces with v{prompt.version}
		</a>
		<a class="text-accent underline underline-offset-2" href={filtered()}>
			Traces with any version
		</a>
	</p>
</div>
