// A zlib stream inflated from inside a larger buffer, saying where it ended.
//
// A pack writes its objects one after another, each a zlib stream with nothing saying how long it is
// compressed, so the next object starts where the inflater stopped reading. The browser's
// DecompressionStream inflates but never says where its input ended, so this is RFC 1950 and 1951 as
// puff.c writes them: slower than zlib, which a workflow's repository is small enough not to notice.

const order = [16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15];
const lengthBase = [3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31, 35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258];
const lengthExtra = [0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0];
const distBase = [1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193, 257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577];
const distExtra = [0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13];

type Huffman = { counts: Uint16Array; symbols: Uint16Array };

function huffman(lengths: ArrayLike<number>, n: number): Huffman {
  const counts = new Uint16Array(16);
  for (let i = 0; i < n; i++) counts[lengths[i]!]!++;
  counts[0] = 0;
  const offsets = new Uint16Array(16);
  for (let len = 1; len < 16; len++) offsets[len] = offsets[len - 1]! + counts[len - 1]!;
  const symbols = new Uint16Array(n);
  for (let i = 0; i < n; i++) if (lengths[i]) symbols[offsets[lengths[i]!]!++] = i;
  return { counts, symbols };
}

const fixed = (() => {
  const lengths = new Uint8Array(288);
  lengths.fill(8, 0, 144).fill(9, 144, 256).fill(7, 256, 280).fill(8, 280, 288);
  return { lit: huffman(lengths, 288), dist: huffman(new Uint8Array(30).fill(5), 30) };
})();

class Bits {
  bitbuf = 0;
  bitcnt = 0;
  constructor(
    readonly src: Uint8Array,
    public pos: number,
  ) {}

  need(n: number): number {
    let val = this.bitbuf;
    while (this.bitcnt < n) {
      if (this.pos >= this.src.length) throw new Error("the compressed object ends before its last block");
      val |= this.src[this.pos++]! << this.bitcnt;
      this.bitcnt += 8;
    }
    this.bitbuf = val >>> n;
    this.bitcnt -= n;
    return val & ((1 << n) - 1);
  }

  decode(h: Huffman): number {
    let code = 0;
    let first = 0;
    let index = 0;
    for (let len = 1; len < 16; len++) {
      code |= this.need(1);
      const count = h.counts[len]!;
      if (code - count < first) return h.symbols[index + (code - first)]!;
      index += count;
      first += count;
      first <<= 1;
      code <<= 1;
    }
    throw new Error("the compressed object holds a code no table has");
  }
}

class Out {
  buf: Uint8Array;
  len = 0;
  constructor(size: number) {
    this.buf = new Uint8Array(Math.max(size, 64));
  }
  room(n: number) {
    if (this.len + n <= this.buf.length) return;
    const next = new Uint8Array(Math.max(this.buf.length * 2, this.len + n));
    next.set(this.buf.subarray(0, this.len));
    this.buf = next;
  }
  push(b: number) {
    this.room(1);
    this.buf[this.len++] = b;
  }
}

// inflate reads the zlib stream that starts at start, its two-byte header, its blocks and its
// Adler-32, and answers what it held and the offset just past it. size, where known, is what it
// holds, which saves growing the output.
export function inflate(src: Uint8Array, start: number, size = 0): { data: Uint8Array; end: number } {
  const cmf = src[start];
  const flg = src[start + 1];
  if (cmf === undefined || flg === undefined || (cmf & 0x0f) !== 8 || ((cmf << 8) | flg) % 31 !== 0) throw new Error("an object of the pack is not zlib compressed");
  if (flg & 0x20) throw new Error("an object of the pack names a preset dictionary, which git never writes");
  const s = new Bits(src, start + 2);
  const out = new Out(size);
  let last = 0;
  while (!last) {
    last = s.need(1);
    const kind = s.need(2);
    if (kind === 0) {
      s.bitbuf = 0;
      s.bitcnt = 0;
      const len = src[s.pos]! | (src[s.pos + 1]! << 8);
      const nlen = src[s.pos + 2]! | (src[s.pos + 3]! << 8);
      if (len !== (~nlen & 0xffff)) throw new Error("a stored block of the pack says two lengths");
      s.pos += 4;
      if (s.pos + len > src.length) throw new Error("the compressed object ends inside a stored block");
      out.room(len);
      out.buf.set(src.subarray(s.pos, s.pos + len), out.len);
      out.len += len;
      s.pos += len;
    } else if (kind === 1) {
      codes(s, out, fixed.lit, fixed.dist);
    } else if (kind === 2) {
      const nlen = s.need(5) + 257;
      const ndist = s.need(5) + 1;
      const ncode = s.need(4) + 4;
      const lengths = new Uint8Array(320);
      for (let i = 0; i < ncode; i++) lengths[order[i]!] = s.need(3);
      const lencode = huffman(lengths, 19);
      lengths.fill(0);
      let i = 0;
      while (i < nlen + ndist) {
        const sym = s.decode(lencode);
        if (sym < 16) {
          lengths[i++] = sym;
          continue;
        }
        let len = 0;
        let repeat: number;
        if (sym === 16) {
          if (i === 0) throw new Error("a block of the pack repeats a length before any");
          len = lengths[i - 1]!;
          repeat = 3 + s.need(2);
        } else if (sym === 17) repeat = 3 + s.need(3);
        else repeat = 11 + s.need(7);
        if (i + repeat > nlen + ndist) throw new Error("a block of the pack writes too many lengths");
        while (repeat--) lengths[i++] = len;
      }
      codes(s, out, huffman(lengths.subarray(0, nlen), nlen), huffman(lengths.subarray(nlen, nlen + ndist), ndist));
    } else {
      throw new Error("a block of the pack is of a kind deflate has not");
    }
  }
  // Past the last block, the stream ends on a whole byte with its Adler-32.
  return { data: out.buf.subarray(0, out.len), end: s.pos + 4 };
}

function codes(s: Bits, out: Out, lit: Huffman, dist: Huffman) {
  for (;;) {
    const sym = s.decode(lit);
    if (sym < 256) {
      out.push(sym);
      continue;
    }
    if (sym === 256) return;
    const l = sym - 257;
    if (l >= 29) throw new Error("a block of the pack names a length deflate has not");
    const len = lengthBase[l]! + s.need(lengthExtra[l]!);
    const d = s.decode(dist);
    if (d >= 30) throw new Error("a block of the pack names a distance deflate has not");
    const back = distBase[d]! + s.need(distExtra[d]!);
    if (back > out.len) throw new Error("a block of the pack refers back before its start");
    out.room(len);
    for (let k = 0; k < len; k++) {
      out.buf[out.len] = out.buf[out.len - back]!;
      out.len++;
    }
  }
}
