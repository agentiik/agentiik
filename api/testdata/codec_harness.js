// Run by TestThePagesConversionsAreWhatTheAPIWritesAndReads, after the page's codec.js, page.js
// wrapped in a function that is never called, which is how its syntax is checked outside a
// browser, and the vectors the test wrote. It prints, in one line of JSON, what codec made of each.
// It runs on node and on macOS's jsc, which prints with print and has no console.
const say = typeof print === "function" ? print : (line) => console.log(line);

const hex = (buffer) => Array.from(new Uint8Array(buffer), (b) => b.toString(16).padStart(2, "0")).join("");

function bytes(h) {
  const out = new Uint8Array(h.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(h.slice(i * 2, i * 2 + 2), 16);
  }
  return out.buffer;
}

// described is what codec made with every ArrayBuffer in it written {"hex": ...}, which JSON carries.
function described(v) {
  if (v instanceof ArrayBuffer) {
    return { hex: hex(v) };
  }
  if (Array.isArray(v)) {
    return v.map(described);
  }
  if (v && typeof v === "object") {
    const o = {};
    for (const k of Object.keys(v)) {
      o[k] = described(v[k]);
    }
    return o;
  }
  return v;
}

function decoded(text) {
  try {
    return { hex: hex(codec.decode(text)) };
  } catch (e) {
    return { refused: String(e && e.message ? e.message : e) };
  }
}

// registration and assertion are credentials as a browser answers them, their bytes ArrayBuffers
// and the registration's convenience methods, which is what codec reads.
function registration(v) {
  return {
    id: v.id,
    rawId: bytes(v.rawId),
    type: "public-key",
    authenticatorAttachment: v.authenticatorAttachment,
    response: {
      clientDataJSON: bytes(v.clientDataJSON),
      attestationObject: bytes(v.attestationObject),
      getAuthenticatorData: () => bytes(v.authenticatorData),
      getTransports: () => v.transports,
      getPublicKey: () => bytes(v.publicKey),
      getPublicKeyAlgorithm: () => v.publicKeyAlgorithm,
    },
    getClientExtensionResults: () => ({ credProps: { rk: true } }),
  };
}

function assertion(v) {
  return {
    id: v.id,
    rawId: bytes(v.rawId),
    type: "public-key",
    authenticatorAttachment: v.authenticatorAttachment,
    response: {
      clientDataJSON: bytes(v.clientDataJSON),
      authenticatorData: bytes(v.authenticatorData),
      signature: bytes(v.signature),
      userHandle: bytes(v.userHandle),
    },
    getClientExtensionResults: () => ({}),
  };
}

// A view of some bytes that starts past the start of its buffer, as a Uint8Array handed over may.
const padded = new Uint8Array(bytes("ff" + vectors.view + "ff"));
const view = padded.subarray(1, padded.length - 1);

say(JSON.stringify({
  encoded: vectors.bytes.map((h) => codec.encode(bytes(h))),
  view: codec.encode(view),
  decoded: vectors.texts.map(decoded),
  creation: described(codec.creationOptions(vectors.creation)),
  request: described(codec.requestOptions(vectors.request)),
  registration: codec.credentialJSON(registration(vectors.registration)),
  assertion: codec.credentialJSON(assertion(vectors.assertion)),
  parsed: typeof pageScript === "function",
}));
