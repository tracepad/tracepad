<script lang="ts">
	import Plus from '@lucide/svelte/icons/plus';
	import { ApiError, api } from '$lib/api/client.svelte';
	import { orderLabels } from '$lib/prompts';
	import { project } from '$lib/project.svelte';
	import Button from '../Button.svelte';
	import ConfirmDialog from '../ConfirmDialog.svelte';
	import LabelChip from './LabelChip.svelte';

	// The label control on a version view (spec 021 #6). Moving `production` is
	// a deploy — spec 003 made a label move *the* release — so a move and a
	// removal each cost one more click, in a dialog that names what moves:
	// *production: v6 → v7* is what a rollback reads before confirming.
	//
	// Attaching a label that points nowhere is immediate: a new label is a note,
	// and a dialog for a note is ceremony.

	let {
		name,
		version,
		/** The labels on this version. */
		labels,
		/**
		 * Every label of the name and where it points (spec 021 #12), or
		 * `null` while no version listing has answered yet — which is not the
		 * same as a name with no labels, and reading it as one is how a *move*
		 * came to happen without the dialog (spec 021 #15).
		 */
		named,
		onchanged
	}: {
		name: string;
		version: number;
		labels: string[];
		named: Record<string, number> | null;
		onchanged: () => void;
	} = $props();

	/** An act waiting for its dialog: what it is, and the sentence that names it. */
	type Pending = { run: () => Promise<unknown>; title: string; description: string; verb: string };

	let pending = $state.raw<Pending | null>(null);
	let adding = $state(false);
	let wanted = $state('');
	let failure = $state<string | null>(null);

	/**
	 * Whether this control may write at all (spec 021 #15). Every write here
	 * depends on knowing where the name's labels point — a move has to be
	 * told apart from a first attachment, and a removal has to be worth
	 * asking about — so until a listing has answered, both controls are shut
	 * and say why. "Answered" is the test, never "the map is non-empty": a
	 * name with no labels looks exactly like a read that has not happened.
	 */
	const shut = $derived(named === null);
	const why = 'The labels of this prompt have not loaded, so nothing here can be changed yet.';

	/** The name's other labels, offered before a new one is typed. */
	const known = $derived(
		orderLabels(Object.keys(named ?? {}).filter((label) => !labels.includes(label)))
	);

	async function run(act: () => Promise<unknown>) {
		failure = null;
		try {
			await act();
			onchanged();
		} catch (cause) {
			failure = cause instanceof ApiError ? cause.message : 'The label did not move.';
		}
	}

	function add(event: SubmitEvent) {
		event.preventDefault();
		const label = wanted.trim();
		// Belt as well as braces: the button is disabled, and the form can
		// still be submitted by a return key in the field.
		if (label === '' || named === null) return;
		// `Object.hasOwn`, not `named[label]`: `toString`, `constructor` and
		// `valueOf` are all label names the server accepts, and a plain lookup
		// finds them on `Object.prototype` — which read as a label already
		// pointing somewhere and opened a dialog offering to move a function
		// (found in review of PR #40).
		const at = Object.hasOwn(named, label) ? named[label] : undefined;
		wanted = '';
		adding = false;
		if (at === undefined) return void run(() => api.putPromptLabel(name, label, version));
		if (at === version) return;
		// It is somewhere else, so this is a deploy: say where from and where to.
		pending = {
			run: () => api.putPromptLabel(name, label, version),
			title: `Move ${label}`,
			description: `${label}: v${at} → v${version}. Whatever fetches this prompt by ${label} gets version ${version} from now on — no new version, no redeploy.`,
			verb: `Move ${label} here`
		};
	}

	function remove(label: string) {
		if (named === null) return;
		pending = {
			run: () => api.deletePromptLabel(name, label),
			title: `Remove ${label}`,
			description: `Remove ${label} from v${version}. Anything fetching this prompt by ${label} stops resolving until the label points somewhere again.`,
			verb: `Remove ${label}`
		};
	}
</script>

<div class="flex flex-wrap items-center gap-1.5">
	{#each orderLabels(labels) as label (label)}
		<!-- A viewer reads where a label points and does not move it (spec 028
		     #15): the chip stays, the cross on it does not. -->
		{#if project.editor}
			<LabelChip {label} onremove={() => remove(label)} disabled={shut} reason={why} />
		{:else}
			<LabelChip {label} />
		{/if}
	{/each}

	{#if !project.editor}
		<!-- Everything below this line writes, so a viewer sees the chips and
		     stops there. -->
	{:else if adding && !shut}
		<form onsubmit={add} class="flex items-center gap-1">
			<!-- svelte-ignore a11y_autofocus -->
			<input
				id="prompt-label-{version}"
				name="label"
				list="prompt-labels-{version}"
				bind:value={wanted}
				autofocus
				autocomplete="off"
				spellcheck="false"
				placeholder="production"
				aria-label="Label to add"
				class="border-border bg-canvas text-fg w-36 rounded-md border px-2 py-0.5 text-xs"
			/>
			<datalist id="prompt-labels-{version}">
				{#each known as label (label)}
					<option value={label}></option>
				{/each}
			</datalist>
			<Button type="submit" variant="primary" disabled={wanted.trim() === ''}>Add</Button>
			<Button onclick={() => ((adding = false), (wanted = ''))}>Cancel</Button>
		</form>
	{:else}
		<Button
			variant="ghost"
			disabled={shut}
			title={shut ? why : undefined}
			onclick={() => (adding = true)}
		>
			<Plus class="size-3.5" />
			Add label…
		</Button>
	{/if}
</div>

{#if failure}
	<p role="alert" class="text-danger mt-1 text-xs">{failure}</p>
{/if}

<ConfirmDialog
	open={pending !== null}
	title={pending?.title ?? ''}
	description={pending?.description ?? ''}
	confirmLabel={pending?.verb ?? ''}
	onconfirm={async () => {
		// Not through `run`: a rejection has to reach the dialog, which keeps
		// it open with the server's reason on it.
		await pending?.run();
		failure = null;
		onchanged();
	}}
	onclose={() => (pending = null)}
/>
