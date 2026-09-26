import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { dirname, join, sep } from 'node:path';
import type { Plugin } from 'vite';

/**
 * The licence of every npm package that reaches the client bundle, as one
 * text file beside it (spec 020 #17). Only modules the bundle renders count —
 * a dependency tree-shaken to nothing ships nothing — and a package with no
 * licence file fails the build rather than going unnamed.
 * `scripts/notices` puts this file after the Go modules' in THIRD_PARTY_NOTICES.
 */
export function notices(): Plugin {
	return {
		name: 'tracepad-notices',
		apply: 'build',
		generateBundle(_, bundle) {
			if (this.environment.name !== 'client') return;
			const roots = new Set<string>();
			for (const output of Object.values(bundle)) {
				if (output.type === 'asset') {
					// CSS a compiler inlined (Tailwind's `@import`) is no module of the
					// bundle; the `/*! name vX | License */` banner it keeps is the trace.
					for (const [, name] of String(output.source).matchAll(/\/\*! (\S+) v\S+ \|/g)) {
						roots.add(root(join(process.cwd(), 'node_modules', name, 'package.json')));
					}
					continue;
				}
				for (const [id, module] of Object.entries(output.modules)) {
					const file = id.replace(/^\0/, '').split('?')[0];
					if (module.renderedLength > 0 && file.includes(`${sep}node_modules${sep}`)) roots.add(root(file));
				}
			}
			const sections = [...roots].map((dir) => {
				const { name, version } = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8'));
				const files = readdirSync(dir).filter((f) => /^(licen[cs]e|copying|notice)/i.test(f)).sort();
				if (files.length === 0) this.error(`${name}@${version} is in the bundle and carries no licence file`);
				const text = files.map((f) => readFileSync(join(dir, f), 'utf8').trim()).join('\n\n');
				return `${name} ${version}\n${'-'.repeat(80)}\n${text}\n`;
			});
			const source = sections.sort().map((s) => `${'='.repeat(80)}\n${s}`).join('\n');
			this.emitFile({ type: 'asset', fileName: 'third-party-notices.txt', source });
		}
	};
}

/** The package a bundled file belongs to: the nearest `package.json` that names one. */
function root(file: string): string {
	for (let dir = dirname(file); ; dir = dirname(dir)) {
		const manifest = join(dir, 'package.json');
		if (existsSync(manifest) && JSON.parse(readFileSync(manifest, 'utf8')).name) return dir;
		if (dirname(dir) === dir) throw new Error(`no package.json above ${file}`);
	}
}
