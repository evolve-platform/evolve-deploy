// Resolve every internal href in dist/ against the page it appears on and check
// that the page — and the anchor, when there is one — is actually there.
// Relative links are the whole reason this exists: they survive a change of
// `base`, and they break silently.
import { readdirSync, readFileSync, existsSync } from 'node:fs';
import { join, posix } from 'node:path';

const DIST = 'dist';
// Read from the built output rather than repeated here: the two drifting apart
// would make this pass while every link on the deployed site is wrong. The
// hashed asset directory is the one prefix every page is guaranteed to carry,
// and it is also empty when the site is served from a domain root.
const BASE = readFileSync(join(DIST, 'index.html'), 'utf8')
  .match(/href="([^"]*)\/_astro\//)[1];
const files = [];
(function walk(d) {
  for (const e of readdirSync(d, { withFileTypes: true })) {
    const p = join(d, e.name);
    if (e.isDirectory()) walk(p);
    else if (e.name.endsWith('.html')) files.push(p);
  }
})(DIST);

const ids = new Map();
const idsOf = (f) => {
  if (!ids.has(f)) {
    const html = readFileSync(f, 'utf8');
    ids.set(f, new Set([...html.matchAll(/\sid="([^"]+)"/g)].map((m) => m[1])));
  }
  return ids.get(f);
};

let bad = 0, checked = 0;
for (const file of files) {
  const pageUrl = BASE + file.slice(DIST.length).replace(/index\.html$/, '');
  for (const m of readFileSync(file, 'utf8').matchAll(/href="([^"]+)"/g)) {
    const href = m[1];
    if (/^(https?:|mailto:|#)/.test(href)) continue;
    const [path, frag] = href.split('#');
    const url = path.startsWith('/') ? path : posix.resolve(pageUrl, path);
    if (/\.[a-z0-9]+$/i.test(url)) continue;         // assets, sitemap, favicon
    checked++;
    if (!url.startsWith(BASE + '/') && url !== BASE) {
      console.log(`MISSING BASE  ${file} -> ${href}`); bad++; continue;
    }
    const target = join(DIST, url.slice(BASE.length), 'index.html');
    if (!existsSync(target)) {
      console.log(`DEAD PAGE     ${file} -> ${href}`); bad++;
    } else if (frag && !idsOf(target).has(frag)) {
      console.log(`DEAD ANCHOR   ${file} -> ${href}`); bad++;
    }
  }
}
console.log(`\n${checked} internal links checked across ${files.length} pages, ${bad} broken`);
process.exit(bad ? 1 : 0);
