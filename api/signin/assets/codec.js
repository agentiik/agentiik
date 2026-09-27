// The conversions between the JSON the API writes and reads for a passkey ceremony and what
// navigator.credentials takes and answers, for a browser without WebAuthn Level 3's
// PublicKeyCredential.parseCreationOptionsFromJSON(), parseRequestOptionsFromJSON() and
// toJSON(), which page.js prefers where they are. Nothing here touches the page, so that a test
// runs it outside a browser against what the API writes and reads.
"use strict";

const codec = (() => {
  // How WebAuthn's JSON writes bytes: base64url, with no padding.
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  const value = new Map(Array.from(alphabet, (c, i) => [c, i]));

  // bytesOf is the bytes of a BufferSource, an ArrayBuffer or a view of one, as WebAuthn hands them.
  function bytesOf(source) {
    if (source instanceof ArrayBuffer) {
      return new Uint8Array(source);
    }
    if (ArrayBuffer.isView(source)) {
      return new Uint8Array(source.buffer, source.byteOffset, source.byteLength);
    }
    throw new TypeError("not bytes: an ArrayBuffer or a view of one was expected");
  }

  // encode writes bytes as base64url with no padding.
  function encode(source) {
    const b = bytesOf(source);
    let out = "";
    for (let i = 0; i < b.length; i += 3) {
      const n = (b[i] << 16) | ((i + 1 < b.length ? b[i + 1] : 0) << 8) | (i + 2 < b.length ? b[i + 2] : 0);
      out += alphabet[(n >> 18) & 63] + alphabet[(n >> 12) & 63];
      if (i + 1 < b.length) {
        out += alphabet[(n >> 6) & 63];
      }
      if (i + 2 < b.length) {
        out += alphabet[n & 63];
      }
    }
    return out;
  }

  // decode reads base64url with no padding into an ArrayBuffer, in the one spelling the API writes:
  // padding, a character of another alphabet, a length no bytes encode to and a bit set past the
  // last byte are refused, as Go's base64.RawURLEncoding.Strict() refuses them, so that the page and
  // the API never read one string as two different things.
  function decode(text) {
    if (typeof text !== "string" || text.length % 4 === 1) {
      throw new TypeError("not base64url: a string of a length some bytes encode to was expected");
    }
    const out = new Uint8Array(Math.floor(text.length * 3 / 4));
    let n = 0;
    let bits = 0;
    let j = 0;
    for (let i = 0; i < text.length; i++) {
      const v = value.get(text[i]);
      if (v === undefined) {
        throw new TypeError("not base64url: it holds a character other than A to Z, a to z, 0 to 9, - and _");
      }
      n = (n << 6) | v;
      bits += 6;
      if (bits >= 8) {
        bits -= 8;
        out[j++] = (n >> bits) & 255;
        n &= (1 << bits) - 1;
      }
    }
    if (n !== 0) {
      throw new TypeError("not base64url: a bit is set past the last byte, which no encoder writes");
    }
    return out.buffer;
  }

  // descriptor is a PublicKeyCredentialDescriptorJSON with its ID decoded.
  function descriptor(d) {
    return { ...d, id: decode(d.id) };
  }

  // creationOptions is parseCreationOptionsFromJSON(): a registration's options with their bytes
  // decoded, every other member as the API wrote it.
  function creationOptions(json) {
    return {
      ...json,
      challenge: decode(json.challenge),
      user: { ...json.user, id: decode(json.user.id) },
      excludeCredentials: (json.excludeCredentials || []).map(descriptor),
    };
  }

  // requestOptions is parseRequestOptionsFromJSON(), for a sign-in.
  function requestOptions(json) {
    return {
      ...json,
      challenge: decode(json.challenge),
      allowCredentials: (json.allowCredentials || []).map(descriptor),
    };
  }

  // credentialJSON is toJSON(): a RegistrationResponseJSON for what navigator.credentials.create()
  // answered, an AuthenticationResponseJSON for what get() answered. The extensions' results are
  // left empty: the API asks for none and reads none, and a result may hold bytes, which JSON
  // cannot carry as they are.
  function credentialJSON(credential) {
    const r = credential.response;
    const response = { clientDataJSON: encode(r.clientDataJSON) };
    if (r.attestationObject !== undefined) {
      if (typeof r.getAuthenticatorData === "function") {
        response.authenticatorData = encode(r.getAuthenticatorData());
      }
      if (typeof r.getTransports === "function") {
        response.transports = r.getTransports();
      }
      if (typeof r.getPublicKey === "function") {
        const key = r.getPublicKey();
        if (key) {
          response.publicKey = encode(key);
        }
      }
      if (typeof r.getPublicKeyAlgorithm === "function") {
        response.publicKeyAlgorithm = r.getPublicKeyAlgorithm();
      }
      response.attestationObject = encode(r.attestationObject);
    } else {
      response.authenticatorData = encode(r.authenticatorData);
      response.signature = encode(r.signature);
      if (r.userHandle) {
        response.userHandle = encode(r.userHandle);
      }
    }
    const json = { id: credential.id, rawId: encode(credential.rawId), type: credential.type, response };
    if (credential.authenticatorAttachment) {
      json.authenticatorAttachment = credential.authenticatorAttachment;
    }
    json.clientExtensionResults = {};
    return json;
  }

  return { encode, decode, creationOptions, requestOptions, credentialJSON };
})();
