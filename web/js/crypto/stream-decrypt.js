import { AUTH_TAG_LENGTH, MAX_FILENAME_LENGTH, CHUNK_SIZE } from './constants.js';
import { deriveChunkIV } from './utils.js';
import { FileHeader } from './header.js';

// Fetch enough for ~10 encryption chunks at a time (10MB + overhead)
// This keeps memory usage low while minimizing HTTP requests
const FETCH_CHUNK_SIZE = 10 * 1024 * 1024 + 10 * AUTH_TAG_LENGTH;

// Maximum buffer size before we stop fetching (50MB)
const MAX_BUFFER_SIZE = 50 * 1024 * 1024;

/**
 * Streaming decryption using Range requests for large files
 *
 * Fetches the file in chunks using HTTP Range requests to avoid browser memory limits,
 * decrypts on the fly, and writes directly to a WritableStream.
 *
 * @param {string} url - URL to fetch from
 * @param {number} totalSize - Total file size (from HEAD request)
 * @param {CryptoKey} key - AES-256-GCM key
 * @param {WritableStream} output - Writable stream for decrypted data
 * @param {function} [onProgress] - Progress callback (0-1)
 * @returns {Promise<string>} - Original filename
 */
export async function decryptWithRangeRequests(url, totalSize, key, output, onProgress) {
    const writer = output.getWriter();

    // Use a list of chunks instead of one growing buffer to avoid 2GB limit
    let chunks = [];
    let totalBuffered = 0;
    let fetchOffset = 0;

    // Helper to fetch a range and add to chunks list
    async function fetchRange(start, end) {
        const response = await fetch(url, {
            headers: { 'Range': `bytes=${start}-${end}` },
            cache: 'no-store'
        });

        // Must be 206 Partial Content for Range requests
        if (response.status !== 206) {
            // Server returned full content instead of range - this breaks our byte tracking
            if (response.status === 200) {
                throw new Error('Server does not support Range requests');
            }
            throw new Error(`Range request failed: ${response.status}`);
        }

        const chunk = new Uint8Array(await response.arrayBuffer());
        const expectedSize = end - start + 1;
        if (chunk.length !== expectedSize) {
            throw new Error(`Range response size mismatch: expected ${expectedSize}, got ${chunk.length}`);
        }

        chunks.push(chunk);
        totalBuffered += chunk.length;
    }

    // Helper to ensure we have enough data buffered
    async function ensureBytes(needed) {
        while (totalBuffered < needed && fetchOffset < totalSize) {
            // Only fetch if buffer is below max size
            const fetchSize = Math.min(FETCH_CHUNK_SIZE, totalSize - fetchOffset);
            const end = fetchOffset + fetchSize - 1;
            await fetchRange(fetchOffset, end);
            fetchOffset = end + 1;
        }

        if (totalBuffered < needed) {
            throw new Error('Unexpected end of file');
        }
    }

    // Helper to consume bytes from the chunks list
    function consumeBytes(count) {
        const result = new Uint8Array(count);
        let offset = 0;

        while (offset < count && chunks.length > 0) {
            const chunk = chunks[0];
            const needed = count - offset;

            if (chunk.length <= needed) {
                // Use entire chunk
                result.set(chunk, offset);
                offset += chunk.length;
                totalBuffered -= chunk.length;
                chunks.shift();
            } else {
                // Use partial chunk
                result.set(chunk.subarray(0, needed), offset);
                // Create a copy instead of a view to avoid potential browser bugs with subarray views
                chunks[0] = chunk.slice(needed);
                totalBuffered -= needed;
                offset += needed;
            }
        }

        return result;
    }

    // Helper to peek at bytes without consuming
    function peekBytes(count) {
        const result = new Uint8Array(count);
        let offset = 0;
        let chunkIndex = 0;
        let chunkOffset = 0;

        while (offset < count && chunkIndex < chunks.length) {
            const chunk = chunks[chunkIndex];
            const available = chunk.length - chunkOffset;
            const needed = count - offset;
            const toCopy = Math.min(available, needed);

            result.set(chunk.subarray(chunkOffset, chunkOffset + toCopy), offset);
            offset += toCopy;

            if (chunkOffset + toCopy >= chunk.length) {
                chunkIndex++;
                chunkOffset = 0;
            } else {
                chunkOffset += toCopy;
            }
        }

        return result;
    }

    try {
        // Read header prefix to get filename length
        await ensureBytes(2);
        const prefix = peekBytes(2);
        const filenameLength = (prefix[0] << 8) | prefix[1];

        if (filenameLength > MAX_FILENAME_LENGTH) {
            throw new Error('Invalid file: filename too long');
        }

        // Calculate full header size and read it
        const fullHeaderSize = 2 + filenameLength + 4 + 8 + 12;
        await ensureBytes(fullHeaderSize);

        const headerBytes = consumeBytes(fullHeaderSize);
        const { header } = FileHeader.parse(headerBytes.buffer);
        const { chunkSize, fileSize, baseIV, filename } = header;

        // Handle empty files
        if (fileSize === 0) {
            await writer.close();
            if (onProgress) onProgress(1);
            return filename;
        }

        const chunkCount = FileHeader.calculateChunkCount(fileSize, chunkSize);
        let processedBytes = 0;

        for (let i = 0; i < chunkCount; i++) {
            // Calculate expected chunk size
            const isLastChunk = (i === chunkCount - 1);
            const remainingPlaintext = fileSize - (i * chunkSize);
            const plaintextChunkSize = isLastChunk ? remainingPlaintext : chunkSize;
            const encryptedChunkSize = plaintextChunkSize + AUTH_TAG_LENGTH;

            // Ensure we have the encrypted chunk in buffer
            await ensureBytes(encryptedChunkSize);
            const encryptedChunk = consumeBytes(encryptedChunkSize);

            // Derive IV for this chunk
            const chunkIV = deriveChunkIV(baseIV, i);

            // For the first chunk, include header as AAD
            const dataToDecrypt = encryptedChunk.slice().buffer;

            // Build decrypt params - only include additionalData for chunk 0
            // Chrome requires additionalData to be omitted (not undefined) when not used
            const decryptParams = { name: 'AES-GCM', iv: chunkIV };
            if (i === 0) {
                decryptParams.additionalData = headerBytes.slice().buffer;
            }

            // Decrypt chunk
            try {
                const decryptedChunk = await crypto.subtle.decrypt(
                    decryptParams,
                    key,
                    dataToDecrypt
                );

                // Write directly to output stream
                await writer.write(new Uint8Array(decryptedChunk));
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

        await writer.close();
        return filename;

    } catch (error) {
        await writer.abort(error);
        throw error;
    }
}

/**
 * Get file header info using a small Range request
 *
 * @param {string} url - URL to fetch from
 * @returns {Promise<{filename: string, fileSize: number}>}
 */
export async function getFileInfoFromRange(url) {
    // Fetch first 2KB which should contain the header
    const response = await fetch(url, {
        headers: { 'Range': 'bytes=0-2047' },
        cache: 'no-store'
    });

    if (response.status !== 206) {
        if (response.status === 200) {
            throw new Error('Server does not support Range requests');
        }
        throw new Error(`Failed to fetch file info: ${response.status}`);
    }

    const data = new Uint8Array(await response.arrayBuffer());

    if (data.length < 2) {
        throw new Error('File too small');
    }

    const filenameLength = (data[0] << 8) | data[1];
    const fullHeaderSize = 2 + filenameLength + 4 + 8 + 12;

    if (data.length < fullHeaderSize) {
        throw new Error('Header incomplete in initial fetch');
    }

    const { header } = FileHeader.parse(data.slice(0, fullHeaderSize).buffer);

    return {
        filename: header.filename,
        fileSize: header.fileSize
    };
}
