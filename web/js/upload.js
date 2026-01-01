import { generateKey, exportKey, encryptFile } from './crypto/index.js';
import { MAX_FILE_SIZE } from './crypto/constants.js';

function formatSize(bytes) {
    if (bytes < 1024) return bytes + ' B';
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
    if (bytes < 1024 * 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
    return (bytes / (1024 * 1024 * 1024)).toFixed(2) + ' GB';
}

/**
 * Upload a file with two-phase progress reporting.
 * @param {File} file - The file to upload
 * @param {Function} onProgress - Callback with (phase, progress, message) where:
 *   - phase: 0 = encrypting, 1 = uploading to storage
 *   - progress: 0.0 to 1.0
 *   - message: Human-readable status
 */
export async function uploadFile(file, onProgress) {
    // Check file size before attempting encryption
    if (file.size > MAX_FILE_SIZE) {
        throw new Error('File too large. Maximum size is 5GB.');
    }

    const totalSize = file.size;
    onProgress(0, 0, 'Generating encryption key...');

    // Generate a random encryption key
    const key = await generateKey();
    const exportedKey = await exportKey(key);

    onProgress(0, 0.1, 'Encrypting file...');

    // Encrypt the file with progress reporting
    const encryptedBlob = await encryptFile(file, key, (progress) => {
        const processed = Math.round(totalSize * progress);
        onProgress(0, 0.1 + progress * 0.9, `Encrypting... ${Math.round(progress * 100)}% (${formatSize(processed)} / ${formatSize(totalSize)})`);
    });

    onProgress(1, 0, 'Uploading...');

    // Upload directly to storage with progress tracking
    const result = await uploadDirectToStorage(encryptedBlob, onProgress);

    return {
        fileId: result.file_id,
        key: exportedKey,
        paymentRequest: result.payment_request,
        paymentHash: result.payment_hash,
        amountSats: result.amount_sats
    };
}

/**
 * Upload blob via streaming proxy.
 * 1. Call /api/upload/init to get file ID
 * 2. PUT to /api/upload/{id} (streams through server to storage)
 * 3. Call /api/upload/complete to finalize
 */
async function uploadDirectToStorage(blob, onProgress) {
    // Step 1: Get file ID from server
    onProgress(1, 0, 'Preparing upload...');

    const initResponse = await fetch('/api/upload/init', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ size: blob.size })
    });

    if (!initResponse.ok) {
        const errorText = await initResponse.text();
        throw new Error(errorText || 'Failed to initialize upload');
    }

    const { file_id } = await initResponse.json();

    // Step 2: Stream upload through server proxy
    await new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open('PUT', `/api/upload/${file_id}`);

        xhr.upload.onprogress = (e) => {
            if (e.lengthComputable) {
                const progress = e.loaded / e.total;
                onProgress(1, progress * 0.95, `Uploading... ${Math.round(progress * 100)}% (${formatSize(e.loaded)} / ${formatSize(e.total)})`);
            }
        };

        xhr.onload = () => {
            if (xhr.status >= 200 && xhr.status < 300) {
                resolve();
            } else {
                reject(new Error(`Upload failed: ${xhr.status} ${xhr.statusText}`));
            }
        };

        xhr.onerror = () => {
            reject(new Error('Network error during upload'));
        };

        xhr.send(blob);
    });

    // Step 3: Complete the upload on server
    onProgress(1, 0.98, 'Finalizing...');

    const completeResponse = await fetch('/api/upload/complete', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ file_id, size: blob.size })
    });

    if (!completeResponse.ok) {
        const errorText = await completeResponse.text();
        throw new Error(errorText || 'Failed to complete upload');
    }

    onProgress(1, 1, 'Upload complete');
    return await completeResponse.json();
}

const POLL_INTERVAL_MS = 2000;
const MAX_POLL_DURATION_MS = 15 * 60 * 1000; // 15 minutes (matches server pending timeout)

export function pollPaymentStatus(fileId, onPaid, onTimeout) {
    const startTime = Date.now();
    let timeoutId = null;

    const poll = async () => {
        // Check if we've exceeded max poll duration
        if (Date.now() - startTime > MAX_POLL_DURATION_MS) {
            if (onTimeout) onTimeout();
            return;
        }

        try {
            const response = await fetch(`/api/file/${fileId}/status`);
            if (!response.ok) {
                timeoutId = setTimeout(poll, POLL_INTERVAL_MS);
                return;
            }

            const status = await response.json();
            if (status.paid) {
                onPaid();
                return;
            }
        } catch (e) {
            // Network error - continue polling
        }

        timeoutId = setTimeout(poll, POLL_INTERVAL_MS);
    };

    poll();

    // Return a cancel function
    return () => {
        if (timeoutId) clearTimeout(timeoutId);
    };
}
