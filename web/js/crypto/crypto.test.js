/**
 * Tests for chunked encryption/decryption
 *
 * Run in browser console or with a test runner that supports WebCrypto
 */

import { generateKey, exportKey, importKey, encryptFile, decryptFile } from './index.js';
import { FileHeader } from './header.js';
import { deriveChunkIV, toBase64Url, fromBase64Url } from './utils.js';
import { CHUNK_SIZE, IV_LENGTH, MAX_FILENAME_LENGTH } from './constants.js';

// Test utilities
function assertEqual(actual, expected, message) {
    if (actual !== expected) {
        throw new Error(`${message}: expected ${expected}, got ${actual}`);
    }
}

function assertArrayEqual(actual, expected, message) {
    if (actual.length !== expected.length) {
        throw new Error(`${message}: length mismatch - expected ${expected.length}, got ${actual.length}`);
    }
    for (let i = 0; i < actual.length; i++) {
        if (actual[i] !== expected[i]) {
            throw new Error(`${message}: mismatch at index ${i} - expected ${expected[i]}, got ${actual[i]}`);
        }
    }
}

function createTestFile(size, filename = 'test.bin') {
    const data = new Uint8Array(size);
    crypto.getRandomValues(data);
    return new File([data], filename, { type: 'application/octet-stream' });
}

// Tests
async function testBase64Roundtrip() {
    const original = crypto.getRandomValues(new Uint8Array(32));
    const encoded = toBase64Url(original);
    const decoded = fromBase64Url(encoded);
    assertArrayEqual(decoded, original, 'Base64 roundtrip');
    console.log('✓ Base64 roundtrip');
}

async function testKeyRoundtrip() {
    const key = await generateKey();
    const exported = await exportKey(key);
    const imported = await importKey(exported);

    // Verify by encrypting/decrypting with both keys
    const testData = new Uint8Array([1, 2, 3, 4, 5]);
    const iv = crypto.getRandomValues(new Uint8Array(12));

    const encrypted = await crypto.subtle.encrypt(
        { name: 'AES-GCM', iv },
        key,
        testData
    );

    const decrypted = await crypto.subtle.decrypt(
        { name: 'AES-GCM', iv },
        imported,
        encrypted
    );

    assertArrayEqual(new Uint8Array(decrypted), testData, 'Key roundtrip');
    console.log('✓ Key export/import roundtrip');
}

async function testIVDerivation() {
    const baseIV = new Uint8Array([1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12]);

    const iv0 = deriveChunkIV(baseIV, 0);
    const iv1 = deriveChunkIV(baseIV, 1);
    const iv2 = deriveChunkIV(baseIV, 2);

    // All IVs should be different from each other
    const arraysEqual = (a, b) => a.every((v, i) => v === b[i]);
    if (arraysEqual(iv0, iv1) || arraysEqual(iv1, iv2) || arraysEqual(iv0, iv2)) {
        throw new Error('IV derivation: IVs should be different');
    }

    // Same index should give same IV
    const iv1Again = deriveChunkIV(baseIV, 1);
    assertArrayEqual(iv1Again, iv1, 'IV derivation determinism');

    console.log('✓ IV derivation');
}

async function testHeaderRoundtrip() {
    const original = new FileHeader({
        filename: 'test-file.txt',
        chunkSize: CHUNK_SIZE,
        fileSize: 12345678,
        baseIV: crypto.getRandomValues(new Uint8Array(IV_LENGTH))
    });

    const serialized = original.serialize();
    const { header: parsed, bytesRead } = FileHeader.parse(serialized);

    assertEqual(parsed.filename, original.filename, 'Header filename');
    assertEqual(parsed.chunkSize, original.chunkSize, 'Header chunkSize');
    assertEqual(parsed.fileSize, original.fileSize, 'Header fileSize');
    assertArrayEqual(parsed.baseIV, original.baseIV, 'Header baseIV');
    assertEqual(bytesRead, serialized.length, 'Header bytesRead');

    console.log('✓ Header roundtrip');
}

async function testSmallFileEncryption() {
    const file = createTestFile(1024, 'small.bin'); // 1 KB
    const key = await generateKey();

    const encrypted = await encryptFile(file, key);
    const { blob, filename } = await decryptFile(encrypted, key);

    assertEqual(filename, 'small.bin', 'Small file filename');
    assertEqual(blob.size, 1024, 'Small file size');

    // Verify content
    const originalData = new Uint8Array(await file.arrayBuffer());
    const decryptedData = new Uint8Array(await blob.arrayBuffer());
    assertArrayEqual(decryptedData, originalData, 'Small file content');

    console.log('✓ Small file encryption/decryption');
}

async function testMultiChunkFile() {
    // Create a file slightly larger than 2 chunks
    const size = CHUNK_SIZE * 2 + 12345;
    const file = createTestFile(size, 'multi-chunk.bin');
    const key = await generateKey();

    let progressCalls = 0;
    const encrypted = await encryptFile(file, key, () => progressCalls++);

    // Should have 3 chunks, so 3 progress calls
    assertEqual(progressCalls, 3, 'Encrypt progress calls');

    progressCalls = 0;
    const { blob, filename } = await decryptFile(encrypted, key, () => progressCalls++);

    assertEqual(progressCalls, 3, 'Decrypt progress calls');
    assertEqual(filename, 'multi-chunk.bin', 'Multi-chunk filename');
    assertEqual(blob.size, size, 'Multi-chunk size');

    // Verify content
    const originalData = new Uint8Array(await file.arrayBuffer());
    const decryptedData = new Uint8Array(await blob.arrayBuffer());
    assertArrayEqual(decryptedData, originalData, 'Multi-chunk content');

    console.log('✓ Multi-chunk file encryption/decryption');
}

async function testExactChunkBoundary() {
    // File exactly 2 chunks
    const size = CHUNK_SIZE * 2;
    const file = createTestFile(size, 'exact.bin');
    const key = await generateKey();

    const encrypted = await encryptFile(file, key);
    const { blob, filename } = await decryptFile(encrypted, key);

    assertEqual(blob.size, size, 'Exact boundary size');

    const originalData = new Uint8Array(await file.arrayBuffer());
    const decryptedData = new Uint8Array(await blob.arrayBuffer());
    assertArrayEqual(decryptedData, originalData, 'Exact boundary content');

    console.log('✓ Exact chunk boundary');
}

async function testUnicodeFilename() {
    const file = createTestFile(100, '日本語ファイル名.txt');
    const key = await generateKey();

    const encrypted = await encryptFile(file, key);
    const { filename } = await decryptFile(encrypted, key);

    assertEqual(filename, '日本語ファイル名.txt', 'Unicode filename');
    console.log('✓ Unicode filename');
}

async function testEmptyFile() {
    const file = new File([], 'empty.txt', { type: 'text/plain' });
    const key = await generateKey();

    const encrypted = await encryptFile(file, key);
    const { blob, filename } = await decryptFile(encrypted, key);

    assertEqual(filename, 'empty.txt', 'Empty file filename');
    assertEqual(blob.size, 0, 'Empty file size');

    console.log('✓ Empty file encryption/decryption');
}

async function testHeaderTampering() {
    const file = createTestFile(1024, 'test.bin');
    const key = await generateKey();

    const encrypted = await encryptFile(file, key);
    const encryptedArray = new Uint8Array(await encrypted.arrayBuffer());

    // Tamper with the header (modify the filename)
    encryptedArray[3] = encryptedArray[3] ^ 0xFF;

    const tamperedBlob = new Blob([encryptedArray], { type: 'application/octet-stream' });

    try {
        await decryptFile(tamperedBlob, key);
        throw new Error('Should have thrown');
    } catch (e) {
        if (!e.message.includes('invalid key or corrupted header')) {
            throw new Error('Wrong error for header tampering: ' + e.message);
        }
    }

    console.log('✓ Header tampering detection (AAD)');
}

async function testFilenameTooLong() {
    const longName = 'a'.repeat(MAX_FILENAME_LENGTH + 1) + '.txt';
    const file = new File([new Uint8Array(100)], longName, { type: 'application/octet-stream' });
    const key = await generateKey();

    try {
        await encryptFile(file, key);
        throw new Error('Should have thrown');
    } catch (e) {
        if (!e.message.includes('Filename too long')) {
            throw new Error('Wrong error for long filename: ' + e.message);
        }
    }

    console.log('✓ Filename length validation');
}

async function testWrongKey() {
    const file = createTestFile(1024, 'test.bin');
    const key1 = await generateKey();
    const key2 = await generateKey();

    const encrypted = await encryptFile(file, key1);

    try {
        await decryptFile(encrypted, key2);
        throw new Error('Should have thrown');
    } catch (e) {
        if (!e.message.includes('invalid key or corrupted header')) {
            throw new Error('Wrong error type: ' + e.message);
        }
    }

    console.log('✓ Wrong key detection');
}

// Run all tests
export async function runTests() {
    console.log('Running crypto tests...\n');

    try {
        await testBase64Roundtrip();
        await testKeyRoundtrip();
        await testIVDerivation();
        await testHeaderRoundtrip();
        await testSmallFileEncryption();
        await testMultiChunkFile();
        await testExactChunkBoundary();
        await testUnicodeFilename();
        await testEmptyFile();
        await testHeaderTampering();
        await testFilenameTooLong();
        await testWrongKey();

        console.log('\n✓ All tests passed!');
        return true;
    } catch (e) {
        console.error('\n✗ Test failed:', e.message);
        console.error(e.stack);
        return false;
    }
}

// Auto-run if loaded directly
if (typeof window !== 'undefined') {
    window.runCryptoTests = runTests;
    console.log('Crypto tests loaded. Run window.runCryptoTests() to execute.');
}
