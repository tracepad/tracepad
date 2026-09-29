// The screens of the shell as data, with no imports: what the navigation draws
// (`sections.ts`, which gives each its icon) and what a project switch keeps
// (`project.svelte.ts`) both read this one list, so a screen is added once.
//
// Spec 007 added three screens by adding three rows, and spec 016 added the
// first *group* — three screens that are one topic, filed under a label so a
// seven-item column says what four of them have in common (#1). A screen's
// `href` is one path segment: what a project switch keeps is that segment.

export type Screen = {
	href: string;
	label: string;
	/** A tab of its own on a phone rather than a row in *More* (spec 006 #20). */
	tab?: true;
	/** The label of the group it is filed under. */
	group?: 'Evals';
};

export const SCREENS = [
	// First, because it is the door (spec 034 #1): the screen that was
	// Stats, seventh, is the project's front page.
	{ href: '/dashboard', label: 'Dashboard', tab: true },
	{ href: '/traces', label: 'Traces', tab: true },
	{ href: '/sessions', label: 'Sessions', tab: true },
	// Between the two screens it joins (spec 023 #8): a user is a set of
	// sessions, and the user page is the dashboard for one of them.
	{ href: '/users', label: 'Users', tab: true },
	// Top level, not under *Evals* (spec 021 #1): a prompt is what the
	// application ships, and filing it under the test loop would say it
	// belongs to the eval nouns.
	{ href: '/prompts', label: 'Prompts' },
	{ href: '/datasets', label: 'Datasets', group: 'Evals' },
	{ href: '/runs', label: 'Runs', group: 'Evals' },
	{ href: '/score-configs', label: 'Score configs', group: 'Evals' },
	// Fourth, and last (spec 024 #10): a queue is an eval noun — the design
	// lists it beside datasets and runs — and it is what the section was
	// made to hold.
	{ href: '/queues', label: 'Queues', group: 'Evals' },
	// Fifth, after the queue (spec 025 #9): quality is what evals produce, so
	// it sits with the datasets, the runs and the annotation desk rather than
	// with the traffic on Stats.
	{ href: '/quality', label: 'Quality', group: 'Evals' },
	{ href: '/settings', label: 'Settings' }
] as const satisfies readonly Screen[];
