/**
 * The boxes a listing measures itself by (`bind:contentRect`, spec 006 #22),
 * for jsdom, which lays nothing out and has no `ResizeObserver`.
 * `FakeResizeObserver` reports one width for every element it watches, when it
 * starts to and whenever `boxWidth` says the width changed; with no width
 * given — the state every test starts in — it reports nothing, which is a box
 * that has not been measured.
 */
let width: number | undefined;
const watching = new Set<FakeResizeObserver>();

export class FakeResizeObserver {
	#callback: ResizeObserverCallback;
	#targets = new Set<Element>();

	constructor(callback: ResizeObserverCallback) {
		this.#callback = callback;
		watching.add(this);
	}

	observe(target: Element) {
		this.#targets.add(target);
		this.tell([target]);
	}

	unobserve(target: Element) {
		this.#targets.delete(target);
	}

	disconnect() {
		this.#targets.clear();
	}

	tell(targets: Iterable<Element> = this.#targets) {
		if (width === undefined) return;
		const box = { width, height: 0 };
		this.#callback(
			[...targets].map(
				(target) =>
					({
						target,
						contentRect: box,
						contentBoxSize: [{ inlineSize: width, blockSize: 0 }]
					}) as unknown as ResizeObserverEntry
			),
			this as unknown as ResizeObserver
		);
	}
}

/** Gives every watched box `px` of width, now and until the next call. */
export function boxWidth(px: number) {
	width = px;
	for (const observer of watching) observer.tell();
}

/** Puts every box back to unmeasured, as the next test finds it. */
export function unmeasured() {
	width = undefined;
}
