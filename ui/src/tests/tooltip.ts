/**
 * The tooltip a browser shows over `element`: the `title` of the nearest
 * ancestor-or-self that has one. Reading a wrapper's attribute instead says
 * nothing about what a pointer over the text gets, when a descendant of the
 * wrapper carries a title of its own.
 */
export function tooltipOver(element: Element): string | null {
	return element.closest('[title]')?.getAttribute('title') ?? null;
}
