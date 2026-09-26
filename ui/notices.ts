import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { normalizePath, type Plugin } from 'vite';

/** The files `scripts/notices` reads as a licence, matched the same way. */
const LICENCE = /^(licen[cs]e|copying|notice|patents)/i;

/**
 * The licence of every npm package that reaches the client bundle, as one
 * text file beside it (spec 020 #17). Only modules the bundle renders count —
 * a dependency tree-shaken to nothing ships nothing — and a package with no
 * licence file fails the build rather than going unnamed. The first line is
 * the count; `scripts/notices` puts the rest after the Go modules.
 */
export function notices(): Plugin {
	let root = '';
	return {
		name: 'tracepad-notices',
		apply: 'build',
		configResolved(config) {
			root = config.root;
		},
		generateBundle(_, bundle) {
			if (this.environment.name !== 'client') return;
			const dirs = new Set<string>();
			for (const output of Object.values(bundle)) {
				if (output.type === 'asset') {
					if (!output.fileName.endsWith('.css')) continue;
					// CSS a compiler inlined (Tailwind's `@import`) is no module of the
					// bundle; the `/*! name vX | License */` banner it keeps is the trace.
					for (const [, name] of String(output.source).matchAll(/\/\*! (\S+) v\S+ \|/g)) {
						const dir = join(root, 'node_modules', name);
						if (!existsSync(join(dir, 'package.json')) || manifest(dir).name !== name) {
							this.error(`the bundle's CSS names ${name}, which is not at ${dir}`);
						}
						dirs.add(dir);
					}
					continue;
				}
				for (const [id, module] of Object.entries(output.modules)) {
					const file = normalizePath(id.replace(/^\0/, '').split('?')[0]);
					if (module.renderedLength > 0 && file.includes('/node_modules/')) dirs.add(packageRoot(file));
				}
			}
			const sections = [...dirs].map((dir) => {
				const { name, version } = manifest(dir);
				const files = readdirSync(dir, { withFileTypes: true })
					.filter((entry) => entry.isFile() && LICENCE.test(entry.name))
					.map((entry) => entry.name)
					.sort();
				if (files.length === 0) this.error(`${name}@${version} is in the bundle and carries no licence file`);
				const text = files.map((f) => readFileSync(join(dir, f), 'utf8').trim()).join('\n\n');
				return `${name} ${version}\n${'-'.repeat(80)}\n${text}\n`;
			});
			const body = sections.sort().map((s) => `${'='.repeat(80)}\n${s}`).join('\n');
			this.emitFile({ type: 'asset', fileName: 'third-party-notices.txt', source: `npm packages: ${sections.length}\n${body}` });
		}
	};
}

function manifest(dir: string): { name?: string; version?: string } {
	return JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8'));
}

/** The package a bundled file belongs to: the nearest `package.json` that names one. */
function packageRoot(file: string): string {
	for (let dir = dirname(file); ; dir = dirname(dir)) {
		if (existsSync(join(dir, 'package.json')) && manifest(dir).name) return dir;
		if (dirname(dir) === dir) throw new Error(`no package.json above ${file}`);
	}
}
