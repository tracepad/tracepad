/**
 * Media references (spec 041 #4): what ingest leaves in a payload where an
 * image or a file was. The pure part — finding them in a value and naming
 * them — so the panel's component is only the drawing.
 */

export type MediaRef = {
	tracepad_media: string;
	mime_type: string;
	size: number;
	/** `false` under the project's placeholder setting (#6): no bytes to fetch. */
	stored?: boolean;
};

export function isMediaRef(value: unknown): value is MediaRef {
	if (typeof value !== 'object' || value === null) return false;
	const ref = value as Record<string, unknown>;
	return (
		typeof ref.tracepad_media === 'string' &&
		/^[0-9a-f]{64}$/.test(ref.tracepad_media) &&
		typeof ref.mime_type === 'string'
	);
}

/**
 * Every reference in a payload, once each, in the order they appear. A body a
 * conversation sends twice is one thumbnail; the JSON view still shows both.
 */
export function mediaRefs(value: unknown): MediaRef[] {
	const found = new Map<string, MediaRef>();
	const walk = (node: unknown, depth: number) => {
		if (depth > 64 || typeof node !== 'object' || node === null) return;
		if (isMediaRef(node)) {
			const key = node.tracepad_media + (node.stored === false ? ':placeholder' : '');
			if (!found.has(key)) found.set(key, node);
			return;
		}
		for (const child of Array.isArray(node) ? node : Object.values(node)) walk(child, depth + 1);
	};
	walk(value, 0);
	return [...found.values()];
}

/** Whether the panel can draw it: an image whose bytes were kept. */
export function isPicture(ref: MediaRef): boolean {
	return ref.stored !== false && ref.mime_type.startsWith('image/');
}

/** A file name for a download: the hash's start and the type's extension. */
export function fileName(ref: MediaRef): string {
	const subtype = ref.mime_type.split('/')[1]?.split(/[+;]/)[0] ?? '';
	const extension = /^[a-z0-9.-]{1,10}$/i.test(subtype) ? `.${subtype}` : '';
	return ref.tracepad_media.slice(0, 12) + extension;
}
