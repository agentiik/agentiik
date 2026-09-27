// A QR code encoder, ISO/IEC 18004:2015, for the one thing the sign-in page draws as one: the
// otpauth:// URI of a TOTP generator being enrolled, which an authenticator application scans. The
// page loads nothing from anywhere else, and a library would be more code than this to read, so it
// is written here, as small as the one use allows: byte mode, error correction level M, versions 1
// to 40, the mask chosen by the standard's penalty. Nothing here touches the page, so that a test
// runs it outside a browser against a reference encoder.
"use strict";

const qr = (() => {
  // The error correction codewords of each block, and the number of blocks, at level M, by version
  // from 1 to 40 (Table 9 of the standard).
  const eccPerBlock = [10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26,
    26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28];
  const blocks = [1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16,
    17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49];

  // level M's two bits in the format information.
  const levelM = 0;

  // utf8 is text as the bytes byte mode carries, which every scanner reads as UTF-8.
  function utf8(text) {
    const out = [];
    for (const ch of String(text)) {
      const c = ch.codePointAt(0);
      if (c < 0x80) {
        out.push(c);
      } else if (c < 0x800) {
        out.push(0xc0 | (c >> 6), 0x80 | (c & 63));
      } else if (c < 0x10000) {
        out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
      } else {
        out.push(0xf0 | (c >> 18), 0x80 | ((c >> 12) & 63), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
      }
    }
    return out;
  }

  // rawModules is how many modules of a version carry codewords: all of them but the function
  // patterns, the format information and, from version 7, the version information.
  function rawModules(version) {
    let n = (16 * version + 128) * version + 64;
    if (version >= 2) {
      const align = Math.floor(version / 7) + 2;
      n -= (25 * align - 10) * align - 55;
      if (version >= 7) {
        n -= 36;
      }
    }
    return n;
  }

  function dataCodewords(version) {
    return Math.floor(rawModules(version) / 8) - eccPerBlock[version - 1] * blocks[version - 1];
  }

  // countBits is the length of byte mode's character count.
  function countBits(version) {
    return version < 10 ? 8 : 16;
  }

  // multiply is a product in GF(2^8) modulo the standard's polynomial, x^8 + x^4 + x^3 + x^2 + 1.
  function multiply(x, y) {
    let z = 0;
    for (let i = 7; i >= 0; i--) {
      z = (z << 1) ^ ((z >>> 7) * 0x11d);
      z ^= ((y >>> i) & 1) * x;
    }
    return z;
  }

  // divisor is the generator polynomial of degree n, its coefficients from the highest power down,
  // the leading 1 left out.
  function divisor(n) {
    const d = new Array(n).fill(0);
    d[n - 1] = 1;
    let root = 1;
    for (let i = 0; i < n; i++) {
      for (let j = 0; j < n; j++) {
        d[j] = multiply(d[j], root);
        if (j + 1 < n) {
          d[j] ^= d[j + 1];
        }
      }
      root = multiply(root, 2);
    }
    return d;
  }

  // remainder is the Reed-Solomon error correction of data.
  function remainder(data, d) {
    const r = new Array(d.length).fill(0);
    for (const b of data) {
      const factor = b ^ r.shift();
      r.push(0);
      for (let i = 0; i < d.length; i++) {
        r[i] ^= multiply(d[i], factor);
      }
    }
    return r;
  }

  // codewords is the bytes in byte mode, padded, split into blocks, each followed by its error
  // correction, and interleaved as the standard places them.
  function codewords(bytes, version) {
    const capacity = dataCodewords(version) * 8;
    const bits = [];
    const put = (value, n) => {
      for (let i = n - 1; i >= 0; i--) {
        bits.push((value >>> i) & 1);
      }
    };
    put(4, 4);
    put(bytes.length, countBits(version));
    for (const b of bytes) {
      put(b, 8);
    }
    put(0, Math.min(4, capacity - bits.length));
    put(0, (8 - (bits.length % 8)) % 8);
    const data = [];
    for (let i = 0; i < bits.length; i += 8) {
      let b = 0;
      for (let j = 0; j < 8; j++) {
        b = (b << 1) | bits[i + j];
      }
      data.push(b);
    }
    for (let pad = 0xec; data.length < capacity / 8; pad ^= 0xec ^ 0x11) {
      data.push(pad);
    }

    const count = blocks[version - 1];
    const ecc = eccPerBlock[version - 1];
    const raw = Math.floor(rawModules(version) / 8);
    const short = count - (raw % count);
    const shortLength = Math.floor(raw / count);
    const d = divisor(ecc);
    const split = [];
    for (let i = 0, at = 0; i < count; i++) {
      const length = shortLength - ecc + (i < short ? 0 : 1);
      const block = data.slice(at, at + length);
      at += length;
      split.push({ data: block, ecc: remainder(block, d) });
    }
    const out = [];
    for (let i = 0; i <= shortLength - ecc; i++) {
      for (const block of split) {
        if (i < block.data.length) {
          out.push(block.data[i]);
        }
      }
    }
    for (let i = 0; i < ecc; i++) {
      for (const block of split) {
        out.push(block.ecc[i]);
      }
    }
    return out;
  }

  // alignments is where the alignment patterns' centres are, along either axis.
  function alignments(version) {
    if (version === 1) {
      return [];
    }
    const count = Math.floor(version / 7) + 2;
    const step = version === 32 ? 26 : Math.ceil((version * 4 + 4) / (count * 2 - 2)) * 2;
    const at = [6];
    for (let p = version * 4 + 10; at.length < count; p -= step) {
      at.splice(1, 0, p);
    }
    return at;
  }

  // The eight masks, by the module's row y and column x.
  const masks = [
    (x, y) => (x + y) % 2 === 0,
    (x, y) => y % 2 === 0,
    (x) => x % 3 === 0,
    (x, y) => (x + y) % 3 === 0,
    (x, y) => (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0,
    (x, y) => ((x * y) % 2) + ((x * y) % 3) === 0,
    (x, y) => (((x * y) % 2) + ((x * y) % 3)) % 2 === 0,
    (x, y) => (((x + y) % 2) + ((x * y) % 3)) % 2 === 0,
  ];

  // finderLike is what a row or a column scores for the dark-light-dark-dark-dark-light-dark pattern
  // of a finder, beside four light modules on one side, the border counting as light, since what
  // lies past it is the quiet zone: the standard's rule N3, found as the reference encoder the tests
  // compare with finds it, the search going on after a pattern counted and from its last three
  // modules after one not counted.
  function finderLike(line) {
    const n = line.length;
    const at = (from) => {
      for (let i = from; i + 7 <= n; i++) {
        if (line[i] && !line[i + 1] && line[i + 2] && line[i + 3] && line[i + 4] && !line[i + 5] && line[i + 6]) {
          return i;
        }
      }
      return -1;
    };
    const light = (from, to) => {
      for (let i = from; i < to; i++) {
        if (line[i]) {
          return false;
        }
      }
      return true;
    };
    let score = 0;
    for (let i = at(0); i !== -1;) {
      let next = i + 7;
      if (light(Math.max(i - 4, 0), i) || light(next, Math.min(next + 4, n))) {
        score += 40;
      } else {
        next = i + 4;
      }
      i = at(next);
    }
    return score;
  }

  // penalty is a masked symbol's score under the standard's four rules, the lower the better:
  // runs of five or more of one colour, two by two blocks of one colour, finder-like patterns, and
  // how far the dark modules are from half.
  function penalty(m) {
    const n = m.length;
    let score = 0;
    let dark = 0;
    for (let i = 0; i < n; i++) {
      let rowRun = 0;
      let colRun = 0;
      const column = [];
      for (let j = 0; j < n; j++) {
        const r = m[i][j];
        const c = m[j][i];
        column.push(c);
        dark += r;
        if (j > 0 && r === m[i][j - 1]) {
          rowRun++;
        } else {
          score += rowRun >= 5 ? rowRun - 2 : 0;
          rowRun = 1;
        }
        if (j > 0 && c === m[j - 1][i]) {
          colRun++;
        } else {
          score += colRun >= 5 ? colRun - 2 : 0;
          colRun = 1;
        }
        if (i > 0 && j > 0 && r === m[i][j - 1] && r === m[i - 1][j] && r === m[i - 1][j - 1]) {
          score += 3;
        }
      }
      score += (rowRun >= 5 ? rowRun - 2 : 0) + (colRun >= 5 ? colRun - 2 : 0);
      score += finderLike(m[i]) + finderLike(column);
    }
    // In floating point, as the reference encoder computes it, so that the two choose alike.
    return score + 10 * Math.trunc(Math.abs((dark / (n * n)) * 100 - 50) / 5);
  }

  // bch is value followed by the remainder of its division by the generator poly, of degree degree.
  function bch(value, poly, degree) {
    let r = value;
    for (let i = 0; i < degree; i++) {
      r = (r << 1) ^ ((r >>> (degree - 1)) * poly);
    }
    return (value << degree) | r;
  }

  // encode answers the QR code of text, in the smallest version that holds it at level M, or in
  // version where one is given, and under mask where one is given, the best by the penalty
  // otherwise: its version, its mask and its modules, rows of 1 for dark and 0 for light, with no
  // quiet zone. It throws where the text does not fit.
  function encode(text, { version, mask } = {}) {
    const bytes = utf8(text);
    const fits = (v) => 4 + countBits(v) + bytes.length * 8 <= dataCodewords(v) * 8;
    let v = version;
    if (v === undefined) {
      for (v = 1; v <= 40 && !fits(v); v++);
    }
    if (!(v >= 1 && v <= 40) || !fits(v)) {
      throw new RangeError("the text is " + bytes.length + " bytes, more than a QR code of that version holds");
    }
    const n = v * 4 + 17;
    const m = Array.from({ length: n }, () => new Array(n).fill(0));
    const fixed = Array.from({ length: n }, () => new Array(n).fill(false));
    const set = (x, y, dark) => {
      m[y][x] = dark ? 1 : 0;
      fixed[y][x] = true;
    };

    // The function patterns: the timing patterns, the finders with their separators, the alignment
    // patterns, and the areas the format and the version information hold, reserved light, as the
    // dark module is, until a mask is chosen, since they are what the standard evaluates it on.
    for (let i = 0; i < n; i++) {
      set(6, i, i % 2 === 0);
      set(i, 6, i % 2 === 0);
    }
    for (const [cx, cy] of [[3, 3], [n - 4, 3], [3, n - 4]]) {
      for (let dy = -4; dy <= 4; dy++) {
        for (let dx = -4; dx <= 4; dx++) {
          const x = cx + dx;
          const y = cy + dy;
          if (x >= 0 && x < n && y >= 0 && y < n) {
            const ring = Math.max(Math.abs(dx), Math.abs(dy));
            set(x, y, ring !== 2 && ring !== 4);
          }
        }
      }
    }
    const centres = alignments(v);
    const last = centres.length - 1;
    for (let i = 0; i <= last; i++) {
      for (let j = 0; j <= last; j++) {
        if ((i === 0 && j === 0) || (i === 0 && j === last) || (i === last && j === 0)) {
          continue;
        }
        for (let dy = -2; dy <= 2; dy++) {
          for (let dx = -2; dx <= 2; dx++) {
            set(centres[i] + dx, centres[j] + dy, Math.max(Math.abs(dx), Math.abs(dy)) !== 1);
          }
        }
      }
    }
    for (let i = 0; i < 9; i++) {
      if (i !== 6) {
        set(8, i, false);
        set(i, 8, false);
      }
    }
    for (let i = 0; i < 8; i++) {
      set(8, n - 1 - i, false);
      set(n - 1 - i, 8, false);
    }
    if (v >= 7) {
      for (let i = 0; i < 18; i++) {
        set(n - 11 + (i % 3), Math.floor(i / 3), false);
        set(Math.floor(i / 3), n - 11 + (i % 3), false);
      }
    }

    // The codewords, in two-module columns from the right, upwards and downwards in turn, skipping
    // the vertical timing pattern; the modules left over are the remainder bits, light.
    const words = codewords(bytes, v);
    let bit = 0;
    for (let right = n - 1; right >= 1; right -= 2) {
      if (right === 6) {
        right = 5;
      }
      for (let k = 0; k < n; k++) {
        for (let j = 0; j < 2; j++) {
          const x = right - j;
          const y = ((right + 1) & 2) === 0 ? n - 1 - k : k;
          if (!fixed[y][x] && bit < words.length * 8) {
            m[y][x] = (words[bit >>> 3] >>> (7 - (bit & 7))) & 1;
            bit++;
          }
        }
      }
    }

    const masked = (which) => m.map((row, y) => row.map((dark, x) => (!fixed[y][x] && masks[which](x, y) ? dark ^ 1 : dark)));
    let chosen = mask;
    if (chosen === undefined) {
      let best = Infinity;
      for (let which = 0; which < 8; which++) {
        const score = penalty(masked(which));
        if (score < best) {
          best = score;
          chosen = which;
        }
      }
    }
    const out = masked(chosen);
    const put = (x, y, dark) => {
      out[y][x] = dark ? 1 : 0;
    };

    // The format information, twice, and the dark module; then the version information, twice.
    const format = bch((levelM << 3) | chosen, 0x537, 10) ^ 0x5412;
    const of = (bits, i) => (bits >>> i) & 1;
    for (let i = 0; i <= 5; i++) {
      put(8, i, of(format, i));
    }
    put(8, 7, of(format, 6));
    put(8, 8, of(format, 7));
    put(7, 8, of(format, 8));
    for (let i = 9; i < 15; i++) {
      put(14 - i, 8, of(format, i));
    }
    for (let i = 0; i < 8; i++) {
      put(n - 1 - i, 8, of(format, i));
    }
    for (let i = 8; i < 15; i++) {
      put(8, n - 15 + i, of(format, i));
    }
    put(8, n - 8, 1);
    if (v >= 7) {
      const info = bch(v, 0x1f25, 12);
      for (let i = 0; i < 18; i++) {
        put(n - 11 + (i % 3), Math.floor(i / 3), of(info, i));
        put(Math.floor(i / 3), n - 11 + (i % 3), of(info, i));
      }
    }
    return { version: v, mask: chosen, modules: out };
  }

  return { encode };
})();
