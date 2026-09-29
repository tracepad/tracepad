<script lang="ts">
	import Inbox from '@lucide/svelte/icons/inbox';
	import { page } from '$app/state';
	import CopyButton from '../CopyButton.svelte';

	// A fresh project's first instructions (spec 034 #6): the exporter
	// settings with a copy button and the quickstart link, on the dashboard —
	// the first screen a new project shows — where a page of empty charts
	// would say nothing about what to do next.

	const snippet = $derived(
		'OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf\n' +
			`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=${page.url.origin}/v1/traces\n` +
			'OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer <your project key>"'
	);
</script>

<div class="min-w-0">
	<h2 class="flex items-center gap-2 font-medium">
		<Inbox class="text-subtle size-4" />
		No traces yet
	</h2>
	<p class="text-muted mt-1">Point an OpenTelemetry-instrumented app at this server and reload.</p>
	<div class="border-border bg-surface mt-3 flex items-start gap-2 rounded-md border p-3">
		<pre class="min-w-0 flex-1 overflow-x-auto font-mono text-xs">{snippet}</pre>
		<CopyButton text={() => snippet} label="Copy the exporter settings" />
	</div>
	<p class="text-subtle mt-2 text-xs">
		The key is the one printed when this project was created; nobody else, including this page, can
		read it back.
	</p>
	<a
		class="text-accent mt-3 inline-block underline underline-offset-2"
		href="https://github.com/tracepad/tracepad/blob/main/docs/quickstart.md"
		target="_blank"
		rel="noreferrer"
	>
		Quickstart
	</a>
</div>
