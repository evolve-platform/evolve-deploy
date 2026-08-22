// Derives every logo asset from the one drawing, so the crops and the two
// colour variants cannot drift apart. One-off: run it after replacing the
// source, not as part of the build.
//
//   node scripts/logo.mjs
import sharp from 'sharp';

const SRC = 'src/assets/logo-source.png';

// The source is black ink on an opaque white page. Keying the white out on a
// threshold would leave a hard, aliased edge on every curve; using the ink's own
// darkness as the alpha keeps the antialiasing the drawing already has.
async function ink(input, colour) {
  const { data, info } = await sharp(input)
    .ensureAlpha()
    .raw()
    .toBuffer({ resolveWithObject: true });

  const out = Buffer.alloc(data.length);
  for (let i = 0; i < data.length; i += 4) {
    const luma = (data[i] * 0.299 + data[i + 1] * 0.587 + data[i + 2] * 0.114) | 0;
    out[i] = colour;
    out[i + 1] = colour;
    out[i + 2] = colour;
    out[i + 3] = 255 - luma;
  }
  return sharp(out, { raw: { width: info.width, height: info.height, channels: 4 } })
    .png()
    .toBuffer();
}

// Trim against the white page after cropping, so the result is the drawing's own
// bounding box rather than a set of numbers that break if the source is redrawn.
// Two pipelines rather than one: sharp applies trim before extract whatever
// order they are chained in, which crops the wrong thing.
const trim = (buf) =>
  sharp(buf).trim({ background: '#ffffff', threshold: 12 }).toBuffer();

const { width: W, height: H } = await sharp(SRC).metadata();
const mark = await trim(
  await sharp(SRC)
    .extract({ left: 0, top: 0, width: W, height: Math.round(H * 0.58) })
    .toBuffer()
);
const full = await trim(await sharp(SRC).toBuffer());

for (const [name, buf] of [['mark', mark], ['wordmark', full]]) {
  for (const [theme, colour] of [['light', 0x11], ['dark', 0xff]]) {
    const png = await ink(buf, colour);
    await sharp(png).toFile(`src/assets/${name}-${theme}.png`);
    const { width, height } = await sharp(png).metadata();
    console.log(`${name}-${theme}.png  ${width}x${height}`);
  }
}

// Keep only the largest connected run of ink. Any rectangle tight enough to make
// the `e` fill a 16px tile also clips the neighbouring clouds, and the corner
// fragments that leaves read as dirt rather than as part of the mark. They are
// separate shapes, so this removes them without a hand-tuned crop that would
// break the moment the source is redrawn.
async function largestShape(input) {
  const { data, info } = await sharp(input).ensureAlpha().raw().toBuffer({ resolveWithObject: true });
  const { width: w, height: h } = info;
  const solid = (i) => data[i * 4 + 3] > 40;

  const label = new Int32Array(w * h).fill(-1);
  let best = -1, bestSize = 0;
  for (let start = 0; start < w * h; start++) {
    if (label[start] !== -1 || !solid(start)) continue;
    const id = start;
    let size = 0;
    const stack = [start];
    label[start] = id;
    while (stack.length) {
      const i = stack.pop();
      size++;
      const x = i % w, y = (i / w) | 0;
      for (const [dx, dy] of [[1, 0], [-1, 0], [0, 1], [0, -1]]) {
        const nx = x + dx, ny = y + dy;
        if (nx < 0 || ny < 0 || nx >= w || ny >= h) continue;
        const n = ny * w + nx;
        if (label[n] === -1 && solid(n)) { label[n] = id; stack.push(n); }
      }
    }
    if (size > bestSize) { bestSize = size; best = id; }
  }

  const out = Buffer.from(data);
  for (let i = 0; i < w * h; i++) if (label[i] !== best) out[i * 4 + 3] = 0;
  return sharp(out, { raw: { width: w, height: h, channels: 4 } }).png().trim({ threshold: 1 }).toBuffer();
}

// Favicon: the mark is twice as wide as it is tall, so dropping it whole into a
// square tile leaves it too small to read at 16px. The centre cloud carrying the
// `e` is nearly square on its own and is the part that identifies the thing, so
// the tile gets that, reversed out of the accent purple — legible against a
// light tab strip and a dark one without needing two files.
const { width: mw, height: mh } = await sharp(mark).metadata();
const centre = await trim(
  await sharp(mark)
    .extract({
      left: Math.round(mw * 0.31),
      top: Math.round(mh * 0.33),
      width: Math.round(mw * 0.43),
      height: Math.round(mh * 0.67),
    })
    .flatten({ background: '#ffffff' })
    .toBuffer()
);

const glyph = await sharp(await largestShape(await ink(centre, 0xff)))
  .resize(430, 430, { fit: 'contain', background: { r: 0, g: 0, b: 0, alpha: 0 } })
  .toBuffer();

await sharp({ create: { width: 512, height: 512, channels: 4, background: '#6b34c4' } })
  .composite([{ input: glyph, gravity: 'center' }])
  .png()
  .toFile('public/favicon.png');
console.log('favicon.png  512x512');
