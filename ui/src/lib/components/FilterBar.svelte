<script lang="ts" module>
	import type { FilterName } from '$lib/api/traces';

	type Field = {
		label: string;
		kind: 'text' | 'number' | 'instant' | 'status' | 'tags';
		placeholder?: string;
		hint?: string;
	};

	// `Record<FilterName, …>` is the second half of the parity promise: the
	// list of filters is checked against `openapi.json` by a test, and a filter
	// on that list with no control here does not compile.
	const FIELDS: Record<FilterName, Field> = {
		from: { label: 'From', kind: 'instant', hint: 'Inclusive' },
		to: { label: 'To', kind: 'instant', hint: 'Exclusive' },
		environment: { label: 'Environment', kind: 'text', placeholder: 'production' },
		user_id: { label: 'User', kind: 'text', placeholder: 'Exact user id' },
		session_id: { label: 'Session', kind: 'text', placeholder: 'Exact session id' },
		name: { label: 'Name', kind: 'text', placeholder: 'Exact trace name' },
		tag: {
			label: 'Tags',
			kind: 'tags',
			placeholder: 'one, two',
			hint: 'A trace must carry every tag'
		},
		status: { label: 'Status', kind: 'status' },
		min_cost: { label: 'Min cost', kind: 'number', placeholder: '0.01' }
	};
</script>

<script lang="ts">
	import ListFilter from '@lucide/svelte/icons/list-filter';
	import X from '@lucide/svelte/icons/x';
	import { Popover } from 'bits-ui';
	import { TRACE_FILTERS, filterCount, type TraceFilters } from '$lib/api/traces';
	import Button from './Button.svelte';

	// The filter bar is a mirror of `GET /api/v1/traces` (Application
	// contract). Everything lives in one popover rather than in a row of nine
	// controls: nine is already too wide for a laptop, and the popover is the
	// same answer on a phone (spec 006 #15). What is actually filtering shows
	// as chips in the bar, so the state is never hidden behind a click.

	let { filters, onchange }: { filters: TraceFilters; onchange: (next: TraceFilters) => void } =
		$props();

	let open = $state(false);
	// The popover edits a copy: a listing that re-queried on every keystroke
	// would fight the person typing an id.
	let draft = $state<TraceFilters>({});

	const active = $derived(filterCount(filters));

	function apply(event: SubmitEvent) {
		event.preventDefault();
		onchange(prune(draft));
		open = false;
	}

	function clearAll() {
		draft = {};
		onchange({});
		open = false;
	}

	function drop(name: FilterName) {
		const next = { ...filters };
		delete next[name];
		onchange(next);
	}

	// One cast, in one place: the fields are keyed by filter name, and the
	// value type differs per key in a way the loop cannot narrow.
	const text = (name: FilterName) => (draft[name] as string | undefined) ?? '';
	const set = (name: FilterName, value: string) => {
		(draft as Record<string, unknown>)[name] = value;
	};

	/** Blank fields are not filters; the API refuses a parameter with no value. */
	function prune(input: TraceFilters): TraceFilters {
		const next: TraceFilters = {};
		for (const name of TRACE_FILTERS) {
			const value = input[name];
			if (Array.isArray(value)) {
				const tags = value.map((tag) => tag.trim()).filter(Boolean);
				if (tags.length) next.tag = tags;
			} else if (typeof value === 'string' && value.trim()) {
				if (name === 'status') next.status = value as 'error' | 'ok';
				else (next as Record<string, string>)[name] = value.trim();
			}
		}
		return next;
	}

	/** `datetime-local` speaks local wall time; the API speaks RFC 3339 UTC. */
	function toLocalInput(instant: string): string {
		const at = new Date(instant);
		if (!instant || Number.isNaN(at.getTime())) return '';
		return new Date(at.getTime() - at.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
	}

	function fromLocalInput(value: string): string {
		const at = new Date(value);
		return value && !Number.isNaN(at.getTime()) ? at.toISOString() : '';
	}

	function chipLabel(name: FilterName): string {
		const value = filters[name];
		return `${FIELDS[name].label}: ${Array.isArray(value) ? value.join(', ') : value}`;
	}

	const fieldClass =
		'border-border bg-canvas placeholder:text-subtle w-full rounded-md border px-2 py-1 text-sm';
</script>

<div class="flex min-w-0 items-center gap-1.5">
	<Popover.Root bind:open onOpenChange={(next) => next && (draft = { ...filters })}>
		<Popover.Trigger>
			{#snippet child({ props })}
				<Button {...props}>
					<ListFilter class="size-4" />
					Filters
					{#if active > 0}
						<span class="bg-accent text-on-accent ml-0.5 rounded-full px-1.5 text-xs tabular-nums">
							{active}
						</span>
					{/if}
				</Button>
			{/snippet}
		</Popover.Trigger>
		<Popover.Portal>
			<Popover.Content
				sideOffset={6}
				align="start"
				class="border-border bg-canvas shadow-overlay z-50 w-[min(22rem,calc(100vw-1.5rem))]
					rounded-lg border p-3"
			>
				<form onsubmit={apply}>
					<div class="grid grid-cols-2 gap-x-2 gap-y-2.5">
						{#each TRACE_FILTERS as name (name)}
							{@const field = FIELDS[name]}
							<div class={field.kind === 'tags' ? 'col-span-2' : ''}>
								<label for="filter-{name}" class="text-muted mb-1 block text-xs font-medium">
									{field.label}
								</label>
								{#if field.kind === 'status'}
									<select
										id="filter-{name}"
										value={draft.status ?? ''}
										onchange={(event) => set(name, event.currentTarget.value)}
										class={fieldClass}
									>
										<option value="">Any</option>
										<option value="error">Error</option>
										<option value="ok">OK</option>
									</select>
								{:else if field.kind === 'instant'}
									<input
										id="filter-{name}"
										type="datetime-local"
										value={toLocalInput(text(name))}
										oninput={(event) => set(name, fromLocalInput(event.currentTarget.value))}
										class={fieldClass}
									/>
								{:else if field.kind === 'tags'}
									<input
										id="filter-{name}"
										type="text"
										value={draft.tag?.join(', ') ?? ''}
										oninput={(event) => (draft.tag = event.currentTarget.value.split(','))}
										placeholder={field.placeholder}
										autocomplete="off"
										class={fieldClass}
									/>
								{:else}
									<input
										id="filter-{name}"
										type={field.kind === 'number' ? 'number' : 'text'}
										step={field.kind === 'number' ? 'any' : undefined}
										min={field.kind === 'number' ? '0' : undefined}
										value={text(name)}
										oninput={(event) => set(name, event.currentTarget.value)}
										placeholder={field.placeholder}
										autocomplete="off"
										spellcheck="false"
										class={fieldClass}
									/>
								{/if}
								{#if field.hint}
									<p class="text-subtle mt-0.5 text-xs">{field.hint}</p>
								{/if}
							</div>
						{/each}
					</div>
					<div class="mt-3 flex justify-end gap-1.5">
						<Button onclick={clearAll} disabled={active === 0}>Clear all</Button>
						<Button type="submit" variant="primary">Apply</Button>
					</div>
				</form>
			</Popover.Content>
		</Popover.Portal>
	</Popover.Root>

	<ul class="flex min-w-0 items-center gap-1 overflow-x-auto">
		{#each TRACE_FILTERS as name (name)}
			{#if filters[name] !== undefined}
				<li>
					<button
						type="button"
						onclick={() => drop(name)}
						aria-label="Remove filter {chipLabel(name)}"
						class="border-border bg-surface text-muted hover:bg-raised hover:text-fg
							pointer-coarse:min-h-11 flex max-w-56 cursor-pointer items-center gap-1 rounded-md
							border px-2 py-1 text-sm whitespace-nowrap transition-colors duration-100"
					>
						<span class="truncate">{chipLabel(name)}</span>
						<X class="size-3.5 shrink-0" />
					</button>
				</li>
			{/if}
		{/each}
	</ul>
</div>
