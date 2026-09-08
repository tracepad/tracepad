<script lang="ts">
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import ChevronLeft from '@lucide/svelte/icons/chevron-left';
	import ChevronUp from '@lucide/svelte/icons/chevron-up';
	import LoaderCircle from '@lucide/svelte/icons/loader-circle';
	import Plus from '@lucide/svelte/icons/plus';
	import Trash2 from '@lucide/svelte/icons/trash-2';
	import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
	import { goto } from '$app/navigation';
	import { ApiError, api } from '$lib/api/client.svelte';
	import {
		CUSTOM_ROLE,
		ROLES,
		draftFrom,
		emptyDraft,
		pickRole,
		problems,
		roleAfter,
		versionBody,
		type Draft,
		type PromptType
	} from '$lib/prompts';
	import Button from '../Button.svelte';
	import JsonEditor from '../json/JsonEditor.svelte';
	import LabelChip from './LabelChip.svelte';
	import PageHeader from '../PageHeader.svelte';

	// The editor as a full page (spec 021 #4), for both routes: a new name and
	// a new version of an existing one. An edit *is* a new version — the store
	// is append-only — so there is only ever one editor, and it is "new version
	// from this one".
	//
	// The bodies are textareas: a prompt has no syntax to lint, and the cost of
	// a CodeMirror instance is paid once, for `config`, where JSON is what is
	// being typed. A page rather than a panel, because a chat prompt is several
	// long texts.

	let {
		/** The name being appended to; empty when this creates one (#4). */
		name = '',
		/** The version to prefill from; the latest when absent. */
		from = null,
		/** The name's existing labels, offered as chips to point at this version. */
		known = []
	}: { name?: string; from?: number | null; known?: string[] } = $props();

	const naming = $derived(name === '');

	let draft = $state<Draft>(emptyDraft());
	let loading = $state(false);
	let busy = $state(false);
	let failure = $state<string | null>(null);
	let wanted = $state('');
	/**
	 * The version this editor believes the name is at, which the save sends as
	 * `expect_version` (spec 021 #14): 0 for a name it believes is new, and
	 * otherwise the **latest** — not the `?from=` it is prefilled with, which
	 * is what is being edited rather than what is being appended to. `null`
	 * means the editor has not established it yet, and *Save* waits.
	 */
	let expect = $state.raw<number | null>(null);
	/**
	 * Where the server said the name actually is when it refused the save, and
	 * which name that was about — so that typing a different one is a way out
	 * of "this name is taken" without a round trip, while "a version landed
	 * under you" has only the one way out, which is to reload.
	 */
	let conflict = $state.raw<{ at: number; name: string } | null>(null);
	let reload = $state(0);

	$effect(() => {
		void reload;
		const controller = new AbortController();
		void prefill(name, from, controller.signal);
		return () => controller.abort();
	});

	/**
	 * What the form starts as. A new name starts empty; a new version starts as
	 * a copy of the one it is made from, because that is what "edit this
	 * prompt" means where nothing can be edited in place.
	 *
	 * Two reads when — and only when — an older version is being copied: the
	 * latest establishes the precondition, the named one fills the form. At the
	 * head they are the same request.
	 */
	async function prefill(named: string, version: number | null, signal: AbortSignal) {
		failure = null;
		conflict = null;
		if (named === '') {
			draft = emptyDraft();
			expect = 0;
			return;
		}
		loading = true;
		expect = null;
		try {
			const latest = await api.getPrompt(named, {}, signal);
			const source =
				version === null || version === latest.version
					? latest
					: await api.getPrompt(named, { version }, signal);
			if (signal.aborted) return;
			draft = draftFrom(source);
			expect = latest.version;
		} catch (cause) {
			if (signal.aborted) return;
			failure = cause instanceof ApiError ? cause.message : 'Failed to read the prompt.';
		} finally {
			if (!signal.aborted) loading = false;
		}
	}

	/** The name this editor is writing to. */
	const target = $derived(naming ? draft.name.trim() : name);
	/** Whether the refusal on screen still applies to what is on screen. */
	const refused = $derived(conflict !== null && conflict.name === target);

	const found = $derived(problems(draft, naming));
	const ready = $derived(
		Object.keys(found).length === 0 && expect !== null && !loading && !busy && !refused
	);
	/**
	 * Whether anything has been typed yet. A blank form is not a wrong one:
	 * "a prompt needs a name" in red over a page nobody has touched reads as a
	 * failure rather than as the rule it is, and the dead *Save* already says
	 * the form is not finished. The rules appear at the fields from the first
	 * keystroke on.
	 *
	 * Set by the plain fields rather than by the form: CodeMirror emits an
	 * `input` of its own as it mounts, so a listener on the form would count
	 * the editor's own arrival as typing.
	 */
	let touched = $state(false);
	const type = () => (touched = true);

	function move(i: number, by: -1 | 1) {
		const next = [...draft.messages];
		const [message] = next.splice(i, 1);
		next.splice(i + by, 0, message);
		draft.messages = next;
	}

	function addLabel(event: SubmitEvent) {
		event.preventDefault();
		const label = wanted.trim();
		if (label !== '' && !draft.labels.includes(label)) draft.labels = [...draft.labels, label];
		wanted = '';
	}

	async function save() {
		if (expect === null) return;
		busy = true;
		failure = null;
		conflict = null;
		const to = target;
		try {
			const created = await api.createPromptVersion(to, {
				...versionBody(draft),
				expect_version: expect
			});
			await goto(`/prompts/${encodeURIComponent(to)}?version=${created.version}`);
		} catch (cause) {
			// Including the `404` of a prompt deleted while this page was open
			// (edge cases): the server is the oracle, and it says so here.
			failure = cause instanceof ApiError ? cause.message : 'Failed to save the version.';
			// A `409` means the name is not where this editor thought (#14):
			// somebody else published it, or a version landed while this one
			// was being written. The server says where it is; the offer below
			// is made out of that number rather than out of a guess.
			if (cause instanceof ApiError && cause.status === 409) {
				const at = cause.details.version;
				conflict = { at: typeof at === 'number' ? at : 0, name: to };
			}
		} finally {
			busy = false;
		}
	}

	const back = $derived(naming ? '/prompts' : `/prompts/${encodeURIComponent(name)}`);
	const field = 'border-border bg-canvas text-fg rounded-md border px-2 py-1 text-sm';
	const area = `${field} min-h-24 w-full resize-y font-mono`;
</script>

<svelte:head><title>{naming ? 'New prompt' : `New version · ${name}`} · Tracepad</title></svelte:head>

<PageHeader title={naming ? 'New prompt' : `${name} — new version`}>
	{#snippet meta()}
		<a href={back} class="hover:text-fg flex items-center gap-0.5 whitespace-nowrap">
			<ChevronLeft class="size-3.5" />
			{naming ? 'Prompts' : name}
		</a>
		{#if loading}
			<LoaderCircle class="size-3.5 animate-spin" />
		{:else if !naming}
			<span class="text-subtle text-xs">
				{draft.type} · from v{from ?? 'latest'}
			</span>
		{/if}
	{/snippet}
	{#snippet actions()}
		<Button variant="primary" disabled={!ready} {busy} onclick={save}>
			{#if busy}<LoaderCircle class="size-4 animate-spin" />{/if}
			Save
		</Button>
	{/snippet}
</PageHeader>

<div class="min-h-0 flex-1 overflow-auto p-4">
	<form class="mx-auto flex max-w-3xl flex-col gap-4" onsubmit={(event) => event.preventDefault()}>
		{#if failure}
			<p role="alert" class="text-danger flex items-start gap-2 text-sm">
				<TriangleAlert class="mt-0.5 size-4 shrink-0" />
				{failure}
			</p>
		{/if}

		{#if refused && conflict}
			<!-- Nothing was written, and what to do next depends on which
			     disagreement it was: somebody else owns the name, or somebody
			     else has moved it on. Neither is answered by pressing Save
			     again, which is why it stays shut until this is dealt with —
			     and for a name, typing a different one deals with it. -->
			<p class="text-warn text-sm">
				{#if naming}
					This project already has a prompt called <code class="font-mono">{conflict.name}</code>,
					at v{conflict.at}. Nothing here was saved.
					<a
						class="text-accent underline underline-offset-2"
						href="/prompts/{encodeURIComponent(conflict.name)}?version={conflict.at}"
					>
						Open it
					</a>
					and add a version there, or pick another name above.
				{:else}
					v{conflict.at} landed while this was being written, so nothing here was saved —
					appending now would bury it.
					<button
						type="button"
						class="text-accent cursor-pointer underline underline-offset-2"
						onclick={() => reload++}
					>
						Reload from v{conflict.at}
					</button>
					— what is on this page goes with it, so copy anything worth keeping first.
				{/if}
			</p>
		{/if}

		<!-- `always` for the two that cannot be wrong without somebody having
		     made them so: a config only fails once it holds something, and a
		     label only exists once it was added. -->
		{#snippet problem(key: string, always = false)}
			{#if found[key] && (touched || always)}
				<span class="text-danger text-xs">{found[key]}</span>
			{/if}
		{/snippet}

		{#if naming}
			<label class="flex flex-col gap-1">
				<span class="text-muted text-xs font-medium">Name</span>
				<input
					id="prompt-name"
					name="name"
					bind:value={draft.name}
					oninput={type}
					autocomplete="off"
					spellcheck="false"
					placeholder="support-answer"
					class="{field} max-w-sm font-mono"
				/>
				{@render problem('name')}
			</label>

			<fieldset class="flex flex-col gap-1">
				<legend class="text-muted text-xs font-medium">Type</legend>
				<div class="flex gap-4">
					{#each ['text', 'chat'] as const as choice (choice)}
						<label class="flex items-center gap-1.5 text-sm">
							<input
								type="radio"
								name="type"
								value={choice}
								checked={draft.type === choice}
								onchange={() => (draft.type = choice as PromptType)}
							/>
							{choice}
						</label>
					{/each}
				</div>
				<span class="text-subtle text-xs">
					A name keeps one shape for its whole life: a text prompt is one body, a chat prompt is
					messages.
				</span>
			</fieldset>
		{/if}

		<section class="flex flex-col gap-1.5">
			<h2 class="text-muted text-xs font-medium tracking-wide uppercase">Prompt</h2>
			{#if draft.type === 'text'}
				<textarea
					id="prompt-text"
					name="prompt"
					bind:value={draft.text}
					oninput={type}
					aria-label="Prompt"
					class={area}
				></textarea>
			{:else}
				{#each draft.messages as message, i (i)}
					<div class="border-border bg-surface flex flex-col gap-1.5 rounded-md border p-2">
						<div class="flex flex-wrap items-center gap-1.5">
							<!-- The roles the runtimes name, plus *Custom…* for the
							     string the API will store either way (#16). -->
							<select
								name="role"
								value={message.custom ? CUSTOM_ROLE : message.role}
								onchange={(event) => {
									draft.messages[i] = pickRole(message, event.currentTarget.value);
									type();
								}}
								aria-label="Role of message {i + 1}"
								class="{field} w-32 font-mono"
							>
								{#each ROLES as role (role)}
									<option value={role}>{role}</option>
								{/each}
								<option value={CUSTOM_ROLE}>Custom…</option>
							</select>
							{#if message.custom}
								<input
									name="custom-role"
									bind:value={message.role}
									oninput={type}
									autocomplete="off"
									spellcheck="false"
									placeholder="function"
									aria-label="Custom role of message {i + 1}"
									class="{field} w-28 font-mono"
								/>
							{/if}
							<div class="ml-auto flex items-center gap-0.5">
								<Button
									variant="ghost"
									aria-label="Move message {i + 1} up"
									disabled={i === 0}
									onclick={() => move(i, -1)}
								>
									<ChevronUp class="size-4" />
								</Button>
								<Button
									variant="ghost"
									aria-label="Move message {i + 1} down"
									disabled={i === draft.messages.length - 1}
									onclick={() => move(i, 1)}
								>
									<ChevronDown class="size-4" />
								</Button>
								<Button
									variant="ghost"
									aria-label="Remove message {i + 1}"
									onclick={() =>
										(draft.messages = draft.messages.filter((_, at) => at !== i))}
								>
									<Trash2 class="size-4" />
								</Button>
							</div>
						</div>
						{@render problem(`role:${i}`)}
						<textarea
							name="content"
							bind:value={message.content}
							oninput={type}
							aria-label="Content of message {i + 1}"
							class={area}
						></textarea>
						{@render problem(`content:${i}`)}
					</div>
				{/each}
				<div>
					<!-- The role alternates from the last message's (#16). -->
					<Button
						onclick={() =>
							(draft.messages = [
								...draft.messages,
								{ role: roleAfter(draft.messages), content: '' }
							])}
					>
						<Plus class="size-4" />
						Add message
					</Button>
				</div>
			{/if}
			{@render problem('body')}
		</section>

		<section class="flex flex-col gap-1.5">
			<div class="flex items-baseline gap-2">
				<h2 class="text-muted text-xs font-medium tracking-wide uppercase">Config</h2>
				<span class="text-subtle text-xs">Model parameters your runtime reads. Optional.</span>
			</div>
			<JsonEditor bind:text={draft.config} label="Config" disabled={loading} optional />
			{@render problem('config', true)}
		</section>

		<label class="flex flex-col gap-1">
			<span class="text-muted text-xs font-medium">Commit message</span>
			<input
				id="prompt-commit"
				name="commit_message"
				bind:value={draft.commit}
				autocomplete="off"
				placeholder="why this version exists"
				class={field}
			/>
		</label>

		<div class="flex flex-col gap-1.5">
			<span class="text-muted text-xs font-medium">Labels to point at this version</span>
			<div class="flex flex-wrap items-center gap-1.5">
				{#each draft.labels as label (label)}
					<LabelChip
						{label}
						onremove={() => (draft.labels = draft.labels.filter((one) => one !== label))}
					/>
				{/each}
			</div>
			<div class="flex flex-wrap items-center gap-1.5">
				<input
					id="prompt-label"
					name="label"
					list="prompt-known-labels"
					bind:value={wanted}
					autocomplete="off"
					spellcheck="false"
					placeholder="production"
					aria-label="Label to point at this version"
					onkeydown={(event) => {
						if (event.key === 'Enter') addLabel(event as unknown as SubmitEvent);
					}}
					class="{field} w-40"
				/>
				<datalist id="prompt-known-labels">
					{#each known as label (label)}
						<option value={label}></option>
					{/each}
				</datalist>
				<Button disabled={wanted.trim() === ''} onclick={(event) => addLabel(event as unknown as SubmitEvent)}>
					Add label
				</Button>
			</div>
			{@render problem('labels', true)}
		</div>

		<p class="text-subtle text-xs">
			Saving appends a version — nothing here is edited in place. A label named above moves onto
			the new version as it is created.
		</p>
	</form>
</div>
