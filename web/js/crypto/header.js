import {
    CHUNK_SIZE,
    IV_LENGTH,
    FILENAME_LENGTH_SIZE,
    CHUNK_SIZE_FIELD_SIZE,
    FILE_SIZE_FIELD_SIZE
} from './constants.js';
import { concatBuffers } from './utils.js';

/**
 * File header for encrypted files
 *
 * Format:
 * - Filename length (2 bytes, big-endian)
 * - Filename (variable, UTF-8)
 * - Chunk size (4 bytes, big-endian)
 * - Original file size (8 bytes, big-endian)
 * - Base IV (12 bytes)
 */
export class FileHeader {
    /**
     * @param {Object} options
     * @param {string} options.filename - Original filename
     * @param {number} options.chunkSize - Size of each chunk in bytes
     * @param {number} options.fileSize - Original file size in bytes
     * @param {Uint8Array} options.baseIV - Base IV for chunk IV derivation
     */
    constructor({ filename, chunkSize = CHUNK_SIZE, fileSize, baseIV }) {
        this.filename = filename;
        this.chunkSize = chunkSize;
        this.fileSize = fileSize;
        this.baseIV = baseIV;
    }

    /**
     * Calculate the size of the serialized header
     * @returns {number} - Header size in bytes
     */
    get size() {
        const filenameBytes = new TextEncoder().encode(this.filename);
        return FILENAME_LENGTH_SIZE + filenameBytes.length +
               CHUNK_SIZE_FIELD_SIZE + FILE_SIZE_FIELD_SIZE + IV_LENGTH;
    }

    /**
     * Serialize header to bytes
     * @returns {Uint8Array} - Serialized header
     */
    serialize() {
        const filenameBytes = new TextEncoder().encode(this.filename);

        // Filename length (2 bytes)
        const filenameLengthBuf = new Uint8Array(FILENAME_LENGTH_SIZE);
        new DataView(filenameLengthBuf.buffer).setUint16(0, filenameBytes.length, false);

        // Chunk size (4 bytes)
        const chunkSizeBuf = new Uint8Array(CHUNK_SIZE_FIELD_SIZE);
        new DataView(chunkSizeBuf.buffer).setUint32(0, this.chunkSize, false);

        // File size (8 bytes) - use BigInt for large files
        const fileSizeBuf = new Uint8Array(FILE_SIZE_FIELD_SIZE);
        new DataView(fileSizeBuf.buffer).setBigUint64(0, BigInt(this.fileSize), false);

        return concatBuffers(
            filenameLengthBuf,
            filenameBytes,
            chunkSizeBuf,
            fileSizeBuf,
            this.baseIV
        );
    }

    /**
     * Parse header from bytes
     * @param {ArrayBuffer|Uint8Array} buffer - Buffer containing header
     * @returns {{ header: FileHeader, bytesRead: number }} - Parsed header and bytes consumed
     */
    static parse(buffer) {
        const bytes = buffer instanceof Uint8Array ? buffer : new Uint8Array(buffer);
        const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
        let offset = 0;

        // Filename length
        const filenameLength = view.getUint16(offset, false);
        offset += FILENAME_LENGTH_SIZE;

        // Filename
        const filenameBytes = bytes.slice(offset, offset + filenameLength);
        const filename = new TextDecoder().decode(filenameBytes);
        offset += filenameLength;

        // Chunk size
        const chunkSize = view.getUint32(offset, false);
        offset += CHUNK_SIZE_FIELD_SIZE;

        // File size
        const fileSize = Number(view.getBigUint64(offset, false));
        offset += FILE_SIZE_FIELD_SIZE;

        // Base IV
        const baseIV = bytes.slice(offset, offset + IV_LENGTH);
        offset += IV_LENGTH;

        const header = new FileHeader({ filename, chunkSize, fileSize, baseIV });
        return { header, bytesRead: offset };
    }

    /**
     * Calculate number of chunks for a given file size
     * @param {number} fileSize - File size in bytes
     * @param {number} chunkSize - Chunk size in bytes
     * @returns {number} - Number of chunks
     */
    static calculateChunkCount(fileSize, chunkSize = CHUNK_SIZE) {
        return Math.ceil(fileSize / chunkSize);
    }
}
