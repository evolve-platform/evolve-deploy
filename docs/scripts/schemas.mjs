// Publish the config schema of every release into dist/schema/, as
// v<version>.json and, for the newest, latest.json.
//
// Taken from the tags rather than from the working tree, so that what is
// published is what was released: the schema on main may already accept a key
// that no released binary knows, and an editor that took it from there would
// pass a file the pipeline then refuses. It also means nothing has to be copied
// into this directory when a release is cut.
import { execFileSync } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const DIST = join('dist', 'schema');
const SOURCE = 'internal/schema/schema.json';
const LATEST = 'https://deploy.evolve-platform.com/schema/latest.json';

const git = (...args) =>
  execFileSync('git', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });

// A shallow clone has no tags, and a site built from one would be deployed
// with every schema missing — every editor pointed at it would stop
// validating at once. On a laptop that is only a preview, so it is said and
// not fatal.
if (git('rev-parse', '--is-shallow-repository').trim() === 'true') {
  const msg = 'schemas: this is a shallow clone, so there are no tags to publish schemas from';
  if (process.env.CI) {
    console.error(`${msg}; check out with fetch-depth: 0`);
    process.exit(1);
  }
  console.warn(`${msg}; skipped`);
  process.exit(0);
}

const semver = (tag) => tag.slice(1).split('.').map(Number);
const newer = (a, b) => {
  const [x, y] = [semver(a), semver(b)];
  for (let i = 0; i < 3; i++) if (x[i] !== y[i]) return x[i] - y[i];
  return 0;
};

// Pre-releases are left out: latest.json is what someone who did not pin a
// version gets, and that should never be a release candidate.
const tags = git('tag', '--list', 'v*')
  .split('\n')
  .filter((t) => /^v\d+\.\d+\.\d+$/.test(t))
  .sort(newer);

mkdirSync(DIST, { recursive: true });

let latest = null;
for (const tag of tags) {
  let body;
  try {
    body = git('show', `${tag}:${SOURCE}`);
  } catch {
    // Releases from before the schema existed.
    continue;
  }
  const url = `https://deploy.evolve-platform.com/schema/${tag}.json`;
  writeFileSync(join(DIST, `${tag}.json`), body.replace(`"$id": "${LATEST}"`, `"$id": "${url}"`));
  latest = body;
  console.log(`schemas: ${tag}`);
}

if (latest === null) {
  console.log('schemas: no release has a schema yet');
} else {
  writeFileSync(join(DIST, 'latest.json'), latest);
}
