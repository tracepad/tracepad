#!/usr/bin/env node
// Fail at once when the Node on PATH is not one a package's `engines` admits.
//
//   node scripts/node-engines.mjs <package.json>
//
// A wrong Node does not refuse to run: it installs, builds, and fails
// somewhere downstream — a Node 26 ahead of 24 on PATH failed ninety-eight of
// the interface's tests, and none of them said why. This says why, before
// `npm ci` runs. It reads the range forms this repository's `engines` use —
// `^X`, `>=X`, `X`, each with an optional `.Y.Z`, space-joined comparators
// and alternatives joined by `||` — and refuses any other rather than guess.
//
// Plain syntax on purpose: the Node it runs on may be the wrong one, and an
// old one must still be able to read this far and say so.
import { readFileSync } from 'node:fs';

const file = process.argv[2];
const engines = JSON.parse(readFileSync(file, 'utf8')).engines || {};
const range = engines.node;
if (!range) {
  console.error(`${file} declares no engines.node to check against`);
  process.exit(1);
}

const parse = (version) => {
  const parts = version.split('.').map(Number);
  return [parts[0] || 0, parts[1] || 0, parts[2] || 0];
};
const compare = (a, b) => a[0] - b[0] || a[1] - b[1] || a[2] - b[2];
const have = parse(process.versions.node);

function admits(comparator) {
  const match = /^(\^|>=)?(\d+(?:\.\d+){0,2})$/.exec(comparator);
  if (!match) {
    console.error(`${file}: engines.node "${range}" uses "${comparator}", a form scripts/node-engines.mjs does not read`);
    process.exit(1);
  }
  const want = parse(match[2]);
  if (match[1] === '>=') return compare(have, want) >= 0;
  if (match[1] === '^') return have[0] === want[0] && compare(have, want) >= 0;
  // A bare version: the parts it names must match.
  return match[2].split('.').every((part, i) => have[i] === Number(part));
}

const alternatives = range.split('||').map((alternative) => alternative.trim().split(/\s+/));
if (!alternatives.some((comparators) => comparators.every(admits))) {
  const major = /\d+/.exec(range)[0];
  console.error(`Node ${process.version} is not a Node ${file} supports: engines.node is "${range}".`);
  console.error(`Put a supported one first on PATH and run again — with nvm, \`nvm install ${major} && nvm use ${major}\`.`);
  process.exit(1);
}
