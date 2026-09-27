// Run by TestTheQRCodeIsTheReferenceEncoders, after the page's qr.js and the vectors the test read
// from qr_vectors.json: it encodes each text as the vector asks, in the version and under the mask
// it names where it names them, and prints, in one line of JSON, the version, the mask and the rows
// of each, 1 for dark, for the test to compare with what the reference encoder drew. It runs on node
// and on macOS's jsc, which prints with print and has no console.
const say = typeof print === "function" ? print : (line) => console.log(line);

const results = vectors.cases.map((c) => {
  const options = {};
  if (c.version !== undefined) {
    options.version = c.version;
  }
  if (c.mask !== undefined) {
    options.mask = c.mask;
  }
  try {
    const code = qr.encode(c.text, options);
    return { version: code.version, mask: code.mask, rows: code.modules.map((row) => row.join("")) };
  } catch (e) {
    return { error: String(e && e.message ? e.message : e) };
  }
});
let refused;
try {
  qr.encode("x".repeat(2332));
} catch (e) {
  refused = e instanceof RangeError;
}
say(JSON.stringify({ results, refused }));
