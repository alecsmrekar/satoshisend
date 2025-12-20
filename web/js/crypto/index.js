import { KEY_LENGTH } from './constants.js';
import { toBase64Url, fromBase64Url } from './utils.js';

// Re-export main functions
export { encryptFile } from './encrypt.js';
export { decryptFile } from './decrypt.js';
export { decryptWithRangeRequests, getFileInfoFromRange } from './stream-decrypt.js';

/**
 * Generate a new AES-256-GCM key
 * @returns {Promise<CryptoKey>} - Generated key
 */
export async function generateKey() {
    return await crypto.subtle.generateKey(
        { name: 'AES-GCM', length: KEY_LENGTH },
        true, // extractable
        ['encrypt', 'decrypt']
    );
}

/**
 * Export a key to URL-safe base64 string
 * @param {CryptoKey} key - Key to export
 * @returns {Promise<string>} - URL-safe base64 encoded key
 */
export async function exportKey(key) {
    const rawKey = await crypto.subtle.exportKey('raw', key);
    return toBase64Url(new Uint8Array(rawKey));
}

/**
 * Import a key from URL-safe base64 string
 * @param {string} keyStr - URL-safe base64 encoded key
 * @returns {Promise<CryptoKey>} - Imported key
 */
export async function importKey(keyStr) {
    const keyBytes = fromBase64Url(keyStr);
    return await crypto.subtle.importKey(
        'raw',
        keyBytes,
        { name: 'AES-GCM', length: KEY_LENGTH },
        true,
        ['encrypt', 'decrypt']
    );
}
