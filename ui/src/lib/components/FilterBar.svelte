<script lang="ts" module>
	import type { FilterName } from '$lib/api/traces';
	import { OBSERVATION_TYPES } from '$lib/observations';

	type Field = {
		label: string;
		kind: 'text' | 'number' | 'status' | 'tags' | 'choice' | 'facet';
		placeholder?: string;
		hint?: string;
		/** For `choice`: the values, offered beside an "Any" that clears it. */
		options?: readonly string[];
	};

	/**
	 * Not fields in this popover: the window is the shared range control,
	 * which sits in the bar itself and means the same thing here as it does on
	 * Stats (spec 007 #7), and the search text is the box beside it — a filter
	 * people arrive with, not one they go looking for (spec 011, Application
	 * contract).
	 */
	const ON_THE_BAR: readonly FilterName[] = ['from', 'to', 'q'];

	type FieldName = Exclude<FilterName, 'from' | 'to' | 'q'>;

	// `Record<FieldName, …>` is the second half of the parity promise: the list
	// of filters is checked against `openapi.json` by a test, and a filter on
	// that list with neither a control here nor a place on the bar does not
	// compile.
	const FIELDS: Record<FieldName, Field> = {
		// The three that take a list, offered as the list (spec 027 #6). A
		// checkbox is how a person says *these* rather than *this*, and the
		// values come from `GET /api/v1/facets` for the window in view.
		environment: { label: 'Environment', kind: 'facet' },
		user_id: { label: 'User', kind: 'text', placeholder: 'Exact user id' },
		session_id: { label: 'Session', kind: 'text', placeholder: 'Exact session id' },
		name: { label: 'Name', kind: 'facet' },
		tag: {
			label: 'Tags',
			kind: 'tags',
			placeholder: 'one, two',
			hint: 'A trace must carry every tag'
		},
		status: { label: 'Status', kind: 'status' },
		min_cost: { label: 'Min cost', kind: 'number', placeholder: '0.01' },
		release: { label: 'Release', kind: 'facet' },
		version: { label: 'Version', kind: 'text', placeholder: 'checkout-v9' },
		type: {
			label: 'Type',
			kind: 'choice',
			options: OBSERVATION_TYPES,
			hint: 'Traces containing one'
		},
		prompt: {
			label: 'Prompt',
			kind: 'text',
			placeholder: 'support-answer@7',
			hint: 'Name, or name@version'
		},
		run_id: {
			label: 'Run',
			kind: 'text',
			placeholder: '32 hex characters',
			hint: 'The traces of one eval run'
		},
		item_id: {
			label: 'Item',
			kind: 'text',
			placeholder: '32 hex characters',
			hint: 'The attempts at one case'
		}
	};
</script>

<script lang="ts">
	import ListFilter from '@lucide/svelte/icons/list-filter';
	import X from '@lucide/svelte/icons/x';
	import { Popover } from 'bits-ui';
	import { facetChip, readList, writeList, type FacetField } from '$lib/api/facets';
	import { TRACE_FILTERS, type TraceFilters } from '$lib/api/traces';
	import { FacetValues } from '$lib/facets.svelte';
	import Button from './Button.svelte';
	import FacetField_ from './FacetField.svelte';
	import RangePicker from './RangePicker.svelte';
	import SearchBox from './SearchBox.svelte';

	// The filter bar is a mirror of `GET /api/v1/traces` (Application
	// contract). Everything lives in one popover rather than in a row of nine
	// controls: nine is already too wide for a laptop, and the popover is the
	// same answer on a phone (spec 006 #15). What is actually filtering shows
	// as chips in the bar, so the state is never hidden behind a click.

	let { filters, onchange }: { filters: TraceFilters; onchange: (next: TraceFilters) => void } =
		$props();

	/** The filters this popover owns: everything the bar itself does not. */
	const FIELD_NAMES = TRACE_FILTERS.filter((name) => !ON_THE_BAR.includes(name)) as FieldName[];

	let open = $state(false);
	// The popover edits a copy: a listing that re-queried on every keystroke
	// would fight the person typing an id.
	let draft = $state<TraceFilters>({});
	// Tags are kept as the raw text somebody is typing, not as the parsed
	// array. Round-tripping through `split(',')` and `join(', ')` rewrites the
	// field's value on the keystroke that adds a comma, which sends the caret
	// to the end and makes editing a list in the middle impossible.
	let tagsText = $state('');
	// The values the three facet fields offer, read when the panel opens and
	// again when the window moves under it (spec 027 #6). The listing's first
	// paint pays nothing for it: a minority of visits open the panel.
	const facets = new FacetValues(() => ({ from: filters.from, to: filters.to }), () => open);
	$effect(() => facets.watch());

	// The badge counts what is behind the button, so the window — which is on
	// the bar in plain sight — is not counted twice.
	const active = $derived(
		FIELD_NAMES.filter((name) => {
			const value = filters[name];
			return Array.isArray(value) ? value.length > 0 : Boolean(value);
		}).length
	);

	function edit(opening: boolean) {
		if (!opening) return;
		draft = { ...filters };
		tagsText = filters.tag?.join(', ') ?? '';
	}

	function apply(event: SubmitEvent) {
		event.preventDefault();
		onchange(prune({ ...draft, tag: tagsText.split(',') }));
		open = false;
	}

	// Clears what this popover owns, and only that. The window and the search
	// text are on the bar in plain sight, each with its own control and its own
	// way to be cleared; a button in here that silently reset them would undo
	// something nobody pointed at.
	function clearAll() {
		draft = {};
		tagsText = '';
		const { from, to, q } = filters;
		onchange(prune({ from, to, q }));
		open = false;
	}

	function drop(name: FieldName) {
		const next = { ...filters };
		delete next[name];
		onchange(next);
	}

	/** The window, changed by the shared control, leaving the rest alone. */
	function setRange(range: { from?: string; to?: string }) {
		const { from: _from, to: _to, ...rest } = filters;
		onchange({ ...rest, ...range });
	}

	/** The search text, changed by the box beside it, leaving the rest alone. */
	function setSearch(q: string) {
		const { q: _q, ...rest } = filters;
		onchange(q ? { ...rest, q } : rest);
	}

	// One cast, in one place: the fields are keyed by filter name, and the
	// value type differs per key in a way the loop cannot narrow.
	const text = (name: FieldName) => (draft[name] as string | undefined) ?? '';
	const set = (name: FieldName, value: string) => {
		(draft as Record<string, unknown>)[name] = value;
	};

	// A facet field holds the comma form the URL carries and the API takes
	// (Decision 7), so nothing between the checkbox and the query string parses
	// it twice.
	const picked = (name: FieldName) => readList(text(name));
	const pick = (name: FieldName, values: string[]) => {
		(draft as Record<string, unknown>)[name] = writeList(values) ?? '';
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

	/**
	 * What a chip says and what its tooltip says. They differ only for a
	 * many-valued filter above two values, where the number is the information
	 * and the names stay one hover away (Decision 7).
	 */
	function chip(name: FieldName): { text: string; title: string } {
		const label = FIELDS[name].label;
		const value = filters[name];
		if (FIELDS[name].kind === 'facet' && typeof value === 'string') {
			return facetChip(label, readList(value));
		}
		const text = `${label}: ${Array.isArray(value) ? value.join(', ') : value}`;
		return { text, title: text };
	}

	const fieldClass =
		'border-border bg-canvas placeholder:text-subtle w-full rounded-md border px-2 py-1 text-sm';
</script>

<div class="flex min-w-0 items-center gap-1.5">
	<SearchBox value={filters.q ?? ''} onchange={setSearch} />

	<RangePicker range={{ from: filters.from, to: filters.to }} onchange={setRange} />

	<Popover.Root bind:open onOpenChange={edit}>
		<Popover.Trigger>
			{#snippet child({ props })}
				<!-- `shrink-0`, alone on this bar: the search box and the window
				     can show less of what they hold and still be read, and this
				     button cannot — it is a target and a count. What shrinks is
				     what has something to give (spec 027 #22). -->
				<Button {...props} class="shrink-0">
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
				<!-- The fields scroll and the two buttons do not: with three
				     checkbox lists in it the panel is taller than a laptop's
				     viewport, and Apply below the fold is a panel that cannot be
				     used (spec 027 #6). -->
				<form
					onsubmit={apply}
					class="flex max-h-[calc(var(--bits-popover-content-available-height,100vh)-1.5rem)] flex-col"
				>
					<div class="grid grid-cols-2 gap-x-2 gap-y-2.5 overflow-y-auto px-0.5 py-0.5">
						{#each FIELD_NAMES as name (name)}
							{@const field = FIELDS[name]}
							<div class={field.kind === 'tags' || field.kind === 'facet' ? 'col-span-2' : ''}>
								{#if field.kind === 'facet'}
									<!-- A heading rather than a `<label>`: a facet is a group
									     of checkboxes and not one control, so there is nothing
									     for `for` to point at, and a label that labels no field
									     is one a screen reader announces into the void. The
									     group takes it by `aria-labelledby`. -->
									<p id="filter-{name}-label" class="text-muted mb-1 text-xs font-medium">
										{field.label}
									</p>
									<FacetField_
										id="filter-{name}"
										label={field.label}
										{name}
										values={facets.values[name as FacetField]}
										omitted={facets.omitted[name as FacetField]}
										loading={facets.loading}
										failure={facets.failure}
										checked={picked(name)}
										onchange={(next) => pick(name, next)}
									/>
								{:else}
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
								{:else if field.kind === 'choice'}
									<select
										id="filter-{name}"
										value={text(name)}
										onchange={(event) => set(name, event.currentTarget.value)}
										class={fieldClass}
									>
										<option value="">Any</option>
										{#each field.options ?? [] as option (option)}
											<option value={option}>{option}</option>
										{/each}
									</select>
								{:else if field.kind === 'tags'}
									<input
										id="filter-{name}"
										type="text"
										bind:value={tagsText}
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
								{/if}
								{#if field.hint}
									<p class="text-subtle mt-0.5 text-xs">{field.hint}</p>
								{/if}
							</div>
						{/each}
					</div>
					<div class="mt-3 flex shrink-0 justify-end gap-1.5">
						<Button onclick={clearAll} disabled={active === 0}>Clear all</Button>
						<Button type="submit" variant="primary">Apply</Button>
					</div>
				</form>
			</Popover.Content>
		</Popover.Portal>
	</Popover.Root>

	<ul class="flex min-w-0 items-center gap-1 overflow-x-auto">
		{#each FIELD_NAMES as name (name)}
			{#if filters[name] !== undefined}
				{@const label = chip(name)}
				<li>
					<button
						type="button"
						onclick={() => drop(name)}
						aria-label="Remove filter {label.title}"
						title={label.title}
						class="border-border bg-surface text-muted hover:bg-raised hover:text-fg
							pointer-coarse:min-h-11 flex max-w-56 cursor-pointer items-center gap-1 rounded-md
							border px-2 py-1 text-sm whitespace-nowrap transition-colors duration-100"
					>
						<span class="truncate">{label.text}</span>
						<X class="size-3.5 shrink-0" />
					</button>
				</li>
			{/if}
		{/each}
	</ul>
</div>
