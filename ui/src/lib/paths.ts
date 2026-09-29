// The shape of a project's URL (spec 029 #1, #2): where every screen inside
// the shell lives, and the two ways to write and to read it. It imports
// nothing, so what the navigation reads and what the project reads can both
// read it.

/** Where every screen inside the shell lives. */
export const PREFIX = '/p';

const INSIDE = new RegExp(`^${PREFIX}/[^/]+(/.*)?$`);

/**
 * A path under a project: `under('/traces', id)` is `/p/{id}/traces`, with any
 * query the path carries left where it is.
 */
export function under(path: string, id: string): string {
	return `${PREFIX}/${id}${path}`;
}

/**
 * The path with the project prefix taken off, which is what "which screen is
 * this" compares: `/p/{id}/settings/server` reads as `/settings/server`. A path
 * with no prefix is returned as it is.
 */
export function within(pathname: string): string {
	const match = INSIDE.exec(pathname);
	if (!match) return pathname;
	return match[1] ?? '/';
}
