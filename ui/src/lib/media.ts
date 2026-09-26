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

/**
 * The only extensions a download is named with (spec 041 #32): types whose
 * usual desktop handler is a viewer or a player. The type is the client's
 * claim, and an extension a desktop runs on a double-click — `.hta`, `.exe`,
 * or `.html` and `.svg`, which open in a browser from disk — would turn it
 * into one.
 */
const extensions: Record<string, string> = {
	'image/png': 'png',
	'image/jpeg': 'jpg',
	'image/jpg': 'jpg',
	'image/gif': 'gif',
	'image/webp': 'webp',
	'image/avif': 'avif',
	'image/heic': 'heic',
	'image/heif': 'heif',
	'image/bmp': 'bmp',
	'image/tiff': 'tiff',
	'audio/mpeg': 'mp3',
	'audio/mp3': 'mp3',
	'audio/wav': 'wav',
	'audio/x-wav': 'wav',
	'audio/wave': 'wav',
	'audio/ogg': 'ogg',
	'audio/oga': 'ogg',
	'audio/opus': 'opus',
	'audio/flac': 'flac',
	'audio/aac': 'aac',
	'audio/mp4': 'm4a',
	'audio/webm': 'webm',
	'video/webm': 'webm',
	'video/mp4': 'mp4',
	'video/quicktime': 'mov',
	'video/mpeg': 'mpeg',
	'video/ogg': 'ogv',
	'application/pdf': 'pdf',
	'text/plain': 'txt',
	'application/json': 'json'
};

/**
 * A file name for a download: the hash's start, and an extension only when
 * the type is on the list above — none otherwise, which nothing opens without
 * asking.
 */
export function fileName(ref: MediaRef): string {
	const type = ref.mime_type.split(';')[0].trim().toLowerCase();
	const extension = Object.hasOwn(extensions, type) ? `.${extensions[type]}` : '';
	return ref.tracepad_media.slice(0, 12) + extension;
}
