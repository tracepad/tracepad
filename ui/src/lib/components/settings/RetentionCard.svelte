<script lang="ts">
	import { untrack } from 'svelte';
	import { api, type DryRun, type Project, type RetentionUpdate } from '$lib/api/client.svelte';
	import { project } from '$lib/project.svelte';
	import ConfirmCard from '../ConfirmCard.svelte';
	import Card from './Card.svelte';

	// The retention windows (spec 005). Two numbers, each of which may be
	// absent, and absent means two different things: no window on the queryable
	// data is "keep it forever", while no window on the raw bodies is "follow
	// whatever the queryable window is". The controls say which, because a
	// blank field would say neither.
	//
	// A window that shrinks destroys data on the next sweep, so the server
	// answers with its dry run first and this card renders it. A window that
	// grows is applied on the same click — there is nothing to stop for.

	let { current }: { current: Project } = $props();

	type Mode = 'forever' | 'days';

	// The four controls are seeded from the stored windows and then owned by
	// whoever is editing them: the refresh that follows a successful save must
	// not reach in and rewrite what they are half-way through typing.
	const stored = untrack(() => current);
	let mode = $state<Mode>(stored.retention_days === null ? 'forever' : 'days');
	let days = $state(stored.retention_days ?? 30);
	let rawMode = $state<'follow' | 'days'>(stored.raw_retention_days === null ? 'follow' : 'days');
	let rawDays = $state(stored.raw_retention_days ?? 7);

	/** The PATCH body: both windows, as the endpoint spells them. */
	const body = $derived<RetentionUpdate>({
		retention_days: mode === 'forever' ? null : Math.trunc(days),
		raw_retention_days: rawMode === 'follow' ? null : Math.trunc(rawDays)
	});

	async function send(confirm?: string): Promise<DryRun | string> {
		const answer = await api.patchProject(current.id, body, confirm);
		if ('dry_run' in answer && answer.dry_run) return answer as DryRun;
		await project.refresh();
		return 'Retention updated.';
	}

	const field =
		'border-border bg-canvas w-full rounded-md border px-2 py-1 text-sm disabled:opacity-60';
</script>

<Card
	title="Retention"
	description="How long this project keeps its data. The sweeper runs hourly and deletes what has
		fallen outside the window; shortening one destroys data, so it is previewed first."
>
	<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
		<div>
			<label for="retention-mode" class="text-muted mb-1 block text-xs font-medium">
				Traces, observations and scores
			</label>
			<div class="flex gap-1.5">
				<select id="retention-mode" bind:value={mode} class={field}>
					<option value="forever">Keep forever</option>
					<option value="days">Keep for</option>
				</select>
				<div class="flex items-center gap-1.5">
					<label class="sr-only" for="retention-days">Days of retention</label>
					<input
						id="retention-days"
						type="number"
						min="1"
						max="36500"
						bind:value={days}
						disabled={mode === 'forever'}
						class="{field} w-24"
					/>
					<span class="text-muted text-sm">days</span>
				</div>
			</div>
		</div>

		<div>
			<label for="raw-mode" class="text-muted mb-1 block text-xs font-medium">
				Raw OTLP bodies
			</label>
			<div class="flex gap-1.5">
				<select id="raw-mode" bind:value={rawMode} class={field}>
					<option value="follow">Follow the window above</option>
					<option value="days">Keep for</option>
				</select>
				<div class="flex items-center gap-1.5">
					<label class="sr-only" for="raw-days">Days of raw retention</label>
					<input
						id="raw-days"
						type="number"
						min="1"
						max="36500"
						bind:value={rawDays}
						disabled={rawMode === 'follow'}
						class="{field} w-24"
					/>
					<span class="text-muted text-sm">days</span>
				</div>
			</div>
		</div>
	</div>

	<div class="mt-4">
		<ConfirmCard
			title="Apply the windows"
			description="A window that grows is applied straight away. One that shrinks shows what the
				next sweep would delete, and asks for the project's name before it takes effect."
			echoLabel="project name"
			previewLabel="Save"
			executeLabel="Shorten and delete"
			preview={() => send()}
			execute={(confirm) => send(confirm) as Promise<string>}
		/>
	</div>
</Card>
