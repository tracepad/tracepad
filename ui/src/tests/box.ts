import { vi } from 'vitest';

/**
 * Gives every element `px` of width, which is what a listing measures its box
 * by (`bind:clientWidth`, spec 006 #22); jsdom lays nothing out and reads 0.
 * Undone by `vi.restoreAllMocks()`.
 */
export const boxWidth = (px: number) =>
	vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockReturnValue(px);
