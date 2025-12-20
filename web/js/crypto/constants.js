// Encryption constants
export const CHUNK_SIZE = 1024 * 1024; // 1 MB chunks
export const IV_LENGTH = 12; // 96-bit IV for AES-GCM
export const AUTH_TAG_LENGTH = 16; // 128-bit authentication tag
export const KEY_LENGTH = 256; // AES-256

// Header field sizes
export const FILENAME_LENGTH_SIZE = 2; // 2 bytes for filename length
export const CHUNK_SIZE_FIELD_SIZE = 4; // 4 bytes for chunk size
export const FILE_SIZE_FIELD_SIZE = 8; // 8 bytes for original file size

// Limits
export const MAX_FILE_SIZE = 5 * 1024 * 1024 * 1024; // 5 GB
export const MAX_FILENAME_LENGTH = 1024; // Reasonable limit for filenames
