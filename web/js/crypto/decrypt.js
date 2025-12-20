import { AUTH_TAG_LENGTH, MAX_FILENAME_LENGTH } from './constants.js';
import { deriveChunkIV } from './utils.js';
import { FileHeader } from './header.js';

/**
 * Decrypt an encrypted file blob
 *
 * The first chunk's decryption verifies the header integrity via AAD (Additional Authenticated Data).
 * If the header has been tampered with, decryption of chunk 0 will fail.
 *
 * @param {Blob} encryptedBlob - Encrypted blob with header
 * @param {CryptoKey} key - AES-256-GCM key
 * @param {function} [onProgress] - Progress callback (0-1)
 * @returns {Promise<{ blob: Blob, filename: string }>} - Decrypted blob and original filename
 */
export async function decryptFile(encryptedBlob, key, onProgress) {
    // Read header - first get filename length to calculate full header size
    const minHeaderSize = 2 + 4 + 8 + 12; // filenameLen + chunkSize + fileSize + IV
    const prefixBuffer = await encryptedBlob.slice(0, 2).arrayBuffer();
    const filenameLength = new DataView(prefixBuffer).getUint16(0, false);

    if (filenameLength > MAX_FILENAME_LENGTH) {
        throw new Error('Invalid file: filename too long');
    }

    const fullHeaderSize = 2 + filenameLength + 4 + 8 + 12;
    const headerBuffer = await encryptedBlob.slice(0, fullHeaderSize).arrayBuffer();
    const headerBytes = new Uint8Array(headerBuffer);
    const { header, bytesRead: headerSize } = FileHeader.parse(headerBuffer);

    const { chunkSize, fileSize, baseIV, filename } = header;
    const chunkCount = FileHeader.calculateChunkCount(fileSize, chunkSize);

    // Handle empty files
    if (fileSize === 0) {
        if (onProgress) onProgress(1);
        return { blob: new Blob([], { type: 'application/octet-stream' }), filename };
    }

    // Collect decrypted chunks
    const decryptedParts = [];
    let processedBytes = 0;
    let offset = headerSize;

    for (let i = 0; i < chunkCount; i++) {
        // Calculate expected chunk size (last chunk may be smaller)
        const isLastChunk = (i === chunkCount - 1);
        const remainingPlaintext = fileSize - (i * chunkSize);
        const plaintextChunkSize = isLastChunk ? remainingPlaintext : chunkSize;
        const encryptedChunkSize = plaintextChunkSize + AUTH_TAG_LENGTH;

        // Read encrypted chunk
        const chunkBlob = encryptedBlob.slice(offset, offset + encryptedChunkSize);
        const encryptedChunk = await chunkBlob.arrayBuffer();
        offset += encryptedChunkSize;

        // Derive IV for this chunk
        const chunkIV = deriveChunkIV(baseIV, i);

        // Build decrypt params - only include additionalData for chunk 0
        // Chrome requires additionalData to be omitted (not undefined) when not used
        const decryptParams = { name: 'AES-GCM', iv: chunkIV };
        if (i === 0) {
            decryptParams.additionalData = headerBytes;
        }

        // Decrypt chunk
        try {
            const decryptedChunk = await crypto.subtle.decrypt(
                decryptParams,
                key,
                encryptedChunk
            );
            decryptedParts.push(new Uint8Array(decryptedChunk));
        } catch (error) {
            if (i === 0) {
                throw new Error('Decryption failed: invalid key or corrupted header');
            }
            throw new Error(`Failed to decrypt chunk ${i}: file may be corrupted`);
        }

        // Report progress
        processedBytes += plaintextChunkSize;
        if (onProgress) {
            onProgress(processedBytes / fileSize);
        }
    }

    // Verify total size
    const totalDecrypted = decryptedParts.reduce((sum, p) => sum + p.length, 0);
    if (totalDecrypted !== fileSize) {
        throw new Error(`Size mismatch: expected ${fileSize}, got ${totalDecrypted}`);
    }

    const blob = new Blob(decryptedParts, { type: 'application/octet-stream' });
    return { blob, filename };
}
