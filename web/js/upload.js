import { generateKey, exportKey, encryptFile } from './crypto/index.js';
import { MAX_FILE_SIZE } from './crypto/constants.js';

/**
 * Upload a file with two-phase progress reporting.
 * @param {File} file - The file to upload
 * @param {Function} onProgress - Callback with (phase, progress, message) where:
 *   - phase: 0 = encrypting, 1 = uploading to server, 2 = uploading to secure storage
 *   - progress: 0.0 to 1.0
 *   - message: Human-readable status
 */
export async function uploadFile(file, onProgress) {
    // Check file size before attempting encryption
    if (file.size > MAX_FILE_SIZE) {
        throw new Error('File too large. Maximum size is 5GB.');
    }

    onProgress(0, 0, 'Generating encryption key...');

    // Generate a random encryption key
    const key = await generateKey();
    const exportedKey = await exportKey(key);

    onProgress(0, 0.1, 'Encrypting file...');

    // Encrypt the file with progress reporting
    const encryptedBlob = await encryptFile(file, key, (progress) => {
        onProgress(0, 0.1 + progress * 0.9, `Encrypting file... ${Math.round(progress * 100)}%`);
    });

    onProgress(1, 0, 'Uploading to server...');

    // Upload to server with progress tracking
    const result = await uploadWithProgress(encryptedBlob, onProgress);

    return {
        fileId: result.file_id,
        key: exportedKey,
        paymentRequest: result.payment_request,
        paymentHash: result.payment_hash,
        amountSats: result.amount_sats
    };
}

/**
 * Upload blob to server with two-phase progress tracking.
 * Phase 1: Upload to app server (tracked via XHR upload events)
 * Phase 2: Upload to secure storage (tracked via polling)
 */
async function uploadWithProgress(blob, onProgress) {
    const formData = new FormData();
    formData.append('file', blob, 'encrypted');

    // Phase 1: Upload to server using XHR (for upload progress tracking)
    const uploadResponse = await new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open('POST', '/api/upload');

        let switchedToPhase2 = false;
        xhr.upload.onprogress = (e) => {
            if (e.lengthComputable) {
                const progress = e.loaded / e.total;
                if (progress >= 1 && !switchedToPhase2) {
                    switchedToPhase2 = true;
                    onProgress(2, 0, 'Uploading to secure storage...');
                } else if (!switchedToPhase2) {
                    onProgress(1, progress, `Uploading to server... ${Math.round(progress * 100)}%`);
                }
            }
        };

        xhr.upload.onload = () => {
            if (!switchedToPhase2) {
                switchedToPhase2 = true;
                onProgress(2, 0, 'Uploading to secure storage...');
            }
        };

        xhr.onload = () => {
            if (xhr.status >= 200 && xhr.status < 300) {
                try {
                    resolve(JSON.parse(xhr.responseText));
                } catch (e) {
                    reject(new Error('Invalid response from server'));
                }
            } else {
                reject(new Error(`Upload failed: ${xhr.statusText}`));
            }
        };

        xhr.onerror = () => {
            reject(new Error('Network error during upload'));
        };

        xhr.send(formData);
    });

    // Phase 2: Poll for B2 upload progress
    const uploadId = uploadResponse.upload_id;
    const result = await pollUploadProgress(uploadId, onProgress);
    return result;
}

/**
 * Poll for upload progress until complete or error.
 */
async function pollUploadProgress(uploadId, onProgress) {
    const POLL_INTERVAL = 200; // Poll every 200ms for responsive progress
    const MAX_DURATION = 10 * 60 * 1000; // 10 minute timeout
    const startTime = Date.now();

    while (true) {
        if (Date.now() - startTime > MAX_DURATION) {
            throw new Error('Upload timed out');
        }

        const response = await fetch(`/api/upload/${uploadId}/progress`);
        if (!response.ok) {
            throw new Error('Failed to check upload progress');
        }

        const status = await response.json();

        if (status.status === 'error') {
            throw new Error(status.error || 'Upload failed');
        }

        if (status.status === 'complete') {
            onProgress(2, 1, 'Upload complete');
            return status.result;
        }

        // Update progress
        onProgress(2, status.progress, `Uploading to secure storage... ${Math.round(status.progress * 100)}%`);

        // Wait before next poll
        await new Promise(r => setTimeout(r, POLL_INTERVAL));
    }
}

const POLL_INTERVAL_MS = 2000;
const MAX_POLL_DURATION_MS = 30 * 60 * 1000; // 30 minutes

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
