// monsoon client — connects to /stream and renders delta frames onto a canvas.
//
// Wire format (little-endian):
//   [1]  uint8  flags       0x01 = keyframe
//   [2]  uint16 width
//   [2]  uint16 height
//   [2]  uint16 num_tiles
//   Per tile:
//     [2] uint16 x
//     [2] uint16 y
//     [2] uint16 w
//     [2] uint16 h
//     [4] uint32 jpeg_len
//     [N] JPEG bytes

const canvas = document.getElementById('screen');
const ctx = canvas.getContext('2d', {alpha: false});

// Chain renders so frames are always applied in order and canvas writes
// never race. Newest data always wins because the server drops frames for
// slow clients at the source.
let renderQueue = Promise.resolve();

// connect opens a fetch stream and reads length-prefixed binary frames.
// Each frame is [4-byte LE length][payload]. On any error it reconnects.
async function connect() {
  try {
    const res = await fetch('/stream');
    const reader = res.body.getReader();
    let buf = new Uint8Array(0);

    // readExactly accumulates chunks from the reader until it has n bytes,
    // then returns exactly n bytes and keeps the remainder in buf.
    async function readExactly(n) {
      while (buf.length < n) {
        const {value, done} = await reader.read();
        if (done) throw new Error('stream ended');
        const next = new Uint8Array(buf.length + value.length);
        next.set(buf);
        next.set(value, buf.length);
        buf = next;
      }
      const chunk = buf.slice(0, n); // slice copies, giving chunk its own buffer
      buf = buf.slice(n);
      return chunk;
    }

    while (true) {
      const lenBytes = await readExactly(4);
      const len = new DataView(lenBytes.buffer).getUint32(0, true);
      const msg = await readExactly(len);
      renderQueue = renderQueue
        .then(() => applyFrame(msg.buffer))
        .catch(() => {});
    }
  } catch (_) {
    setTimeout(connect, 1000);
  }
}

// applyFrame parses a binary delta message, decodes all tile JPEGs in
// parallel via createImageBitmap, then paints them onto the canvas.
async function applyFrame(buf) {
  const view = new DataView(buf);
  let off = 0;

  /* flags = */ view.getUint8(off++); // 0x01 = keyframe (informational)
  const width    = view.getUint16(off, true); off += 2;
  const height   = view.getUint16(off, true); off += 2;
  const numTiles = view.getUint16(off, true); off += 2;

  if (canvas.width !== width || canvas.height !== height) {
    canvas.width  = width;
    canvas.height = height;
  }

  // Parse tile metadata and kick off parallel JPEG decodes.
  const positions = new Array(numTiles);
  const decodes   = new Array(numTiles);
  for (let i = 0; i < numTiles; i++) {
    const x   = view.getUint16(off, true); off += 2;
    const y   = view.getUint16(off, true); off += 2;
    const w   = view.getUint16(off, true); off += 2;
    const h   = view.getUint16(off, true); off += 2;
    const len = view.getUint32(off, true); off += 4;

    // Uint8Array view into buf — no copy; Blob holds a reference.
    const jpeg = new Uint8Array(buf, off, len);
    off += len;

    positions[i] = {x, y};
    decodes[i]   = createImageBitmap(new Blob([jpeg], {type: 'image/jpeg'}));
  }

  // Decode all tiles concurrently, then paint in a single synchronous pass.
  const bitmaps = await Promise.all(decodes);
  for (let i = 0; i < bitmaps.length; i++) {
    ctx.drawImage(bitmaps[i], positions[i].x, positions[i].y);
    bitmaps[i].close();
  }
}

connect();
