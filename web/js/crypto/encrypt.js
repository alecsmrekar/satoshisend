import { CHUNK_SIZE, IV_LENGTH, MAX_FILENAME_LENGTH } from './constants.js';
import { deriveChunkIV } from './utils.js';
import { FileHeader } from './header.js';

/**
 * Encrypt a file using chunked AES-256-GCM
 *
 * The first chunk (index 0) includes the header as additional authenticated data (AAD),
 * ensuring the header cannot be tampered with without detection.
 *
 * @param {File} file - File to encrypt
 * @param {CryptoKey} key - AES-256-GCM key
 * @param {function} [onProgress] - Progress callback (0-1)
 * @returns {Promise<Blob>} - Encrypted blob with header
 */
export async function encryptFile(file, key, onProgress) {
    // Validate filename length
    if (file.name.length > MAX_FILENAME_LENGTH) {
        throw new Error(`Filename too long (max ${MAX_FILENAME_LENGTH} characters)`);
    }

    const chunkSize = CHUNK_SIZE;
    const chunkCount = FileHeader.calculateChunkCount(file.size, chunkSize);

    // Generate random base IV
    const baseIV = crypto.getRandomValues(new Uint8Array(IV_LENGTH));

    // Create header
    const header = new FileHeader({
        filename: file.name,
        chunkSize,
        fileSize: file.size,
        baseIV
    });
    const headerBytes = header.serialize();

    // Collect encrypted chunks
    const encryptedParts = [headerBytes];
    let processedBytes = 0;

    // Handle empty files
    if (file.size === 0) {
        if (onProgress) onProgress(1);
        return new Blob(encryptedParts, { type: 'application/octet-stream' });
    }

    for (let i = 0; i < chunkCount; i++) {
        // Read chunk from file
        const start = i * chunkSize;
        const end = Math.min(start + chunkSize, file.size);
        const chunkBlob = file.slice(start, end);
        const chunkData = await chunkBlob.arrayBuffer();

        // Derive IV for this chunk
        const chunkIV = deriveChunkIV(baseIV, i);

        // For the first chunk, include header as AAD to authenticate it
        const aad = (i === 0) ? headerBytes : undefined;

        // Encrypt chunk
        const encryptedChunk = await crypto.subtle.encrypt(
            { name: 'AES-GCM', iv: chunkIV, additionalData: aad },
            key,
            chunkData
        );

        encryptedParts.push(new Uint8Array(encryptedChunk));

        // Report progress
        processedBytes += (end - start);
        if (onProgress) {
            onProgress(processedBytes / file.size);
        }
    }

    // Combine all parts into a single blob
    return new Blob(encryptedParts, { type: 'application/octet-stream' });
}
