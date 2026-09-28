import type { Attachment } from 'svelte/attachments';

/** How far a finger has to carry a sheet before letting go closes it. */
const FAR_ENOUGH = 64;
/** How a sheet let go short of that goes back: a spring, not a jump. */
const SPRING = 'transform 150ms cubic-bezier(0.215, 0.61, 0.355, 1)';

/**
 * Swipe a bottom sheet back down (spec 006 #20). The sheet follows the finger,
 * and letting go further than `FAR_ENOUGH` calls `onaway`; short of it the
 * sheet springs back — at once, for a reader who asked for less motion. A
 * mouse never drags: it has the close button, Escape and the backdrop. A drag
 * starts only on `handle`, because the sheet's list scrolls vertically and
 * cannot also take a vertical drag from anywhere.
 */
export function swipeDown(
	onaway: () => void,
	handle: string,
	still = false
): Attachment<HTMLElement> {
	return (node) => {
		let start: { y: number; id: number } | null = null;
		let travel = 0;

		function down(event: PointerEvent) {
			// One finger drags; a second one on the grip meanwhile is ignored.
			if (start || event.pointerType === 'mouse') return;
			if (!(event.target as Element).closest(handle)) return;
			start = { y: event.clientY, id: event.pointerId };
			travel = 0;
			node.style.transition = '';
		}

		function move(event: PointerEvent) {
			if (!start || event.pointerId !== start.id) return;
			travel = Math.max(0, event.clientY - start.y);
			node.style.transform = `translateY(${travel}px)`;
		}

		function up(event: PointerEvent) {
			if (!start || event.pointerId !== start.id) return;
			start = null;
			if (travel > FAR_ENOUGH && event.type === 'pointerup') {
				// The transform stays: the way out starts where the finger left it.
				onaway();
			} else {
				node.style.transition = still ? '' : SPRING;
				node.style.transform = '';
			}
		}

		node.addEventListener('pointerdown', down);
		node.addEventListener('pointermove', move);
		node.addEventListener('pointerup', up);
		node.addEventListener('pointercancel', up);
		return () => {
			node.removeEventListener('pointerdown', down);
			node.removeEventListener('pointermove', move);
			node.removeEventListener('pointerup', up);
			node.removeEventListener('pointercancel', up);
		};
	};
}
