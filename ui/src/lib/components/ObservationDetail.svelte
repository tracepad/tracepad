<script lang="ts">
	import FileText from '@lucide/svelte/icons/file-text';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { ApiError, api, type Observation, type ObservationIO } from '$lib/api/client.svelte';
	import { ABSENT, duration, elapsed, timestampPrecise, wait } from '$lib/format';
	import CopyButton from './CopyButton.svelte';
	import JsonNode from './JsonNode.svelte';
	import Payload from './Payload.svelte';

	// The right-hand panel: one observation, whole. Everything on it came out
	// of `GET /api/v1/traces/{id}`, except the payloads a budget refused to
	// inline, which are fetched from the one budget-exempt endpoint on demand.

	let {
		observation,
		traceID,
		refused
	}: { observation: Observation; traceID: string; refused: boolean } = $props();

	let full = $state.raw<ObservationIO | null>(null);
	let loading = $state(false);
	let failure = $state<string | null>(null);

	// A different observation is a different payload set; nothing carries over.
	$effect(() => {
		observation.id;
		full = null;
		failure = null;
	});

	async function loadPayloads() {
		if (loading) return;
		loading = true;
		failure = null;
		try {
			full = await api.getObservationIO(observation.id, traceID);
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the payloads.';
		} finally {
			loading = false;
		}
	}

	/** What the endpoint returned wins over what the tree carried. */
	const payload = (key: 'input' | 'output' | 'metadata') =>
		full ? full[key] : (observation as Record<string, unknown>)[key];

	const ms = $derived(elapsed(observation.start_time, observation.end_time));
	const failed = $derived(observation.level === 'ERROR');

	const fields = $derived([
		['Type', observation.type],
		['Started', timestampPrecise(observation.start_time)],
		['Ended', timestampPrecise(observation.end_time)],
		['Duration', duration(ms)],
		// Only when the client reported a completion start: a row saying
		// "TTFT —" on every span that is not a generation is noise in the one
		// block somebody reads line by line (spec 012, Application contract).
		...(observation.ttft_ms == null
			? []
			: ([['TTFT', wait(observation.ttft_ms)]] as const)),
		['Level', observation.level ?? ABSENT],
		['Model', observation.model ?? ABSENT]
	] as const);

	/**
	 * Where the prompt badge leads: the listing filtered by this prompt, at
	 * this version when there is one. It resolves against no registry — the
	 * store may not manage this prompt at all — and the filtered listing needs
	 * none (spec 012 #5).
	 */
	const promptHref = $derived.by(() => {
		const prompt = observation.prompt;
		if (!prompt) return null;
		const label = prompt.version == null ? prompt.name : `${prompt.name}@${prompt.version}`;
		return `/traces?prompt=${encodeURIComponent(label)}`;
	});
</script>

<div class="flex min-h-0 flex-1 flex-col overflow-y-auto">
	<header class="border-border sticky top-0 z-10 border-b px-4 py-3" class:bg-canvas={true}>
		<div class="flex items-start gap-2">
			<div class="min-w-0 flex-1">
				<h2 class="truncate font-medium">{observation.name ?? ABSENT}</h2>
				<p class="text-subtle mt-0.5 flex items-center gap-1 font-mono text-xs">
					<span class="truncate">{observation.id}</span>
					<CopyButton text={observation.id} label="Copy the observation id" />
				</p>
			</div>
		</div>
		{#if observation.prompt && promptHref}
			<!-- The prompt this ran, and a way to ask what else ran it. A badge
			     rather than a row in the timings block: it is the one thing in
			     the panel that leads somewhere. -->
			<p class="mt-2">
				<a
					href={promptHref}
					class="border-border bg-surface text-muted hover:bg-raised hover:text-fg
						inline-flex items-center gap-1 rounded-md border px-2 py-0.5 text-xs
						transition-colors duration-100"
					title="Traces that ran this prompt"
				>
					<FileText class="size-3.5 shrink-0" />
					<span class="truncate">{observation.prompt.name}</span>
					{#if observation.prompt.version != null}
						<span class="text-subtle tabular-nums">v{observation.prompt.version}</span>
					{/if}
				</a>
			</p>
		{/if}
		{#if failed}
			<p
				class="text-danger bg-danger-soft mt-2 flex items-start gap-1.5 rounded-md px-2 py-1.5"
			>
				<TriangleAlert class="mt-0.5 size-4 shrink-0" />
				<span>{observation.status_message ?? 'This observation failed.'}</span>
			</p>
		{:else if observation.status_message}
			<p class="text-muted mt-2">{observation.status_message}</p>
		{/if}
	</header>

	<dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 px-4 py-3">
		{#each fields as [label, value] (label)}
			<dt class="text-subtle text-xs">{label}</dt>
			<dd class="min-w-0 truncate font-mono text-xs" title={String(value)}>{value}</dd>
		{/each}
	</dl>

	{#each [['Usage', observation.usage], ['Cost', observation.cost_details], ['Model parameters', observation.model_parameters]] as const as [label, value] (label)}
		{#if value}
			<section class="border-border border-t px-4 py-3">
				<h3 class="text-muted mb-2 text-xs font-medium tracking-wide uppercase">{label}</h3>
				<div class="overflow-x-auto font-mono text-xs"><JsonNode {value} /></div>
			</section>
		{/if}
	{/each}

	{#if failure}
		<p role="alert" class="text-danger bg-danger-soft border-border border-t px-4 py-2">
			{failure}
		</p>
	{/if}

	{#each [['Input', 'input', observation.input_bytes], ['Output', 'output', observation.output_bytes], ['Metadata', 'metadata', null]] as const as [label, key, size] (key)}
		<Payload
			{label}
			{size}
			value={payload(key)}
			{refused}
			loaded={full !== null}
			{loading}
			onload={loadPayloads}
		/>
	{/each}
</div>
