/**
 * Concatenate multiple Uint8Arrays into one
 * @param {Uint8Array[]} arrays - Arrays to concatenate
 * @returns {Uint8Array} - Concatenated array
 */
export function concatBuffers(...arrays) {
    const totalLength = arrays.reduce((sum, arr) => sum + arr.length, 0);
    const result = new Uint8Array(totalLength);
    let offset = 0;
    for (const arr of arrays) {
        result.set(arr, offset);
        offset += arr.length;
    }
    return result;
}

/**
 * Encode bytes to URL-safe base64
 * @param {Uint8Array} bytes - Bytes to encode
 * @returns {string} - URL-safe base64 string
 */
export function toBase64Url(bytes) {
    const base64 = btoa(String.fromCharCode(...bytes));
    return base64
        .replace(/\+/g, '-')
        .replace(/\//g, '_')
        .replace(/=+$/, '');
}

/**
 * Decode URL-safe base64 to bytes
 * @param {string} str - URL-safe base64 string
 * @returns {Uint8Array} - Decoded bytes
 */
export function fromBase64Url(str) {
    // Restore standard base64
    let base64 = str.replace(/-/g, '+').replace(/_/g, '/');
    // Add padding if needed
    while (base64.length % 4) {
        base64 += '=';
    }
    const binary = atob(base64);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) {
        bytes[i] = binary.charCodeAt(i);
    }
    return bytes;
}

/**
 * Derive a unique IV for a chunk by XORing base IV with chunk index
 * @param {Uint8Array} baseIV - Base IV (12 bytes)
 * @param {number} chunkIndex - Chunk index (0-based)
 * @returns {Uint8Array} - Derived IV (12 bytes)
 */
export function deriveChunkIV(baseIV, chunkIndex) {
    const derived = new Uint8Array(baseIV);
    // XOR the last 4 bytes with chunk index (big-endian)
    const view = new DataView(derived.buffer, derived.byteOffset, derived.byteLength);
    const current = view.getUint32(8, false);
    view.setUint32(8, current ^ chunkIndex, false);
    return derived;
}
