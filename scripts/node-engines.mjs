#!/usr/bin/env node
// Fail at once when the Node on PATH is not one a package's `engines` admits.
//
//   node scripts/node-engines.mjs <package.json>
//
// A wrong Node does not refuse to run: it installs, builds, and fails
// somewhere downstream — a Node 26 ahead of 24 on PATH failed ninety-eight of
// the interface's tests, and none of them said why. This says why, before
// `npm ci` runs.
//
// It reads the range forms npm documents for `engines` — comparators (`>=`,
// `>`, `<=`, `<`, `=`, with or without a space before the version), `^` and
// `~`, partial and x-versions (`22`, `22.x`, `24.1`), hyphen ranges
// (`22 - 26`), space-joined comparators and alternatives joined by `||`.
// Pre-release tags are not read. A range it cannot read is a warning, and the
// check passes: this is a convenience in front of the build, and it must not
// stop a Node the range admits because the script is narrower than npm.
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

const compare = (a, b) => a[0] - b[0] || a[1] - b[1] || a[2] - b[2];
const have = process.versions.node.split('.').map(Number);

// A version as written: its three parts, and how many of them were given
// before the first wildcard.
function parse(text) {
  const match = /^v?(\d+|[xX*])(?:\.(\d+|[xX*]))?(?:\.(\d+|[xX*]))?$/.exec(text);
  if (!match) return null;
  const parts = [match[1], match[2], match[3]];
  let given = 0;
  while (given < 3 && parts[given] !== undefined && /^\d+$/.test(parts[given])) given++;
  const version = [0, 1, 2].map((i) => (i < given ? Number(parts[i]) : 0));
  return { version, given };
}

// The version just past everything a partial one covers: 22 → 23.0.0,
// 24.1 → 24.2.0.
function past({ version, given }) {
  if (given === 0) return null;
  const next = version.slice();
  next[given - 1]++;
  for (let i = given; i < 3; i++) next[i] = 0;
  return next;
}

// A comparator as a test on the running version, or null when unreadable.
function comparator(text) {
  if (text === '' || text === '*' || text === 'x' || text === 'X') return () => true;
  const match = /^(>=|<=|>|<|=|\^|~)?(.*)$/.exec(text);
  const op = match[1] || '';
  const parsed = parse(match[2]);
  if (!parsed) return null;
  const { version, given } = parsed;
  const end = past(parsed);
  const within = (low, high) => compare(have, low) >= 0 && (high === null || compare(have, high) < 0);
  switch (op) {
    case '':
    case '=':
      return given === 3 ? () => compare(have, version) === 0 : () => within(version, end);
    case '>=':
      return () => compare(have, version) >= 0;
    case '>':
      return given === 3 ? () => compare(have, version) > 0 : () => end === null || compare(have, end) >= 0;
    case '<':
      return () => compare(have, version) < 0;
    case '<=':
      return given === 3 ? () => compare(have, version) <= 0 : () => end === null || compare(have, end) < 0;
    case '^':
      return () => within(version, [version[0] + 1, 0, 0]);
    case '~':
      return () => within(version, given >= 2 ? [version[0], version[1] + 1, 0] : [version[0] + 1, 0, 0]);
  }
  return null;
}

// One alternative: a hyphen range, or comparators joined by spaces.
function alternative(text) {
  const hyphen = /^(\S+)\s+-\s+(\S+)$/.exec(text);
  if (hyphen) {
    const low = comparator('>=' + hyphen[1]);
    const high = comparator('<=' + hyphen[2]);
    return low && high ? () => low() && high() : null;
  }
  const tests = text.replace(/(>=|<=|>|<|=|\^|~)\s+/g, '$1').split(/\s+/).map(comparator);
  return tests.every(Boolean) ? () => tests.every((test) => test()) : null;
}

const alternatives = range.split('||').map((text) => alternative(text.trim()));
if (!alternatives.every(Boolean)) {
  console.error(`warning: ${file}: engines.node "${range}" is a form scripts/node-engines.mjs does not read; the Node version is not checked`);
  process.exit(0);
}
if (!alternatives.some((admits) => admits())) {
  const major = /\d+/.exec(range)[0];
  console.error(`Node ${process.version} is not a Node ${file} supports: engines.node is "${range}".`);
  console.error(`Put a supported one first on PATH and run again — with nvm, \`nvm install ${major} && nvm use ${major}\`.`);
  process.exit(1);
}
