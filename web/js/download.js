import { importKey, decryptFile, decryptWithRangeRequests, getFileInfoFromRange } from './crypto/index.js';

// Threshold for using streaming with Range requests (500MB)
const STREAMING_THRESHOLD = 500 * 1024 * 1024;

// Configure StreamSaver to use our local mitm
if (typeof streamSaver !== 'undefined') {
    streamSaver.mitm = '/streamsaver/mitm.html';
}

/**
 * Check if StreamSaver is available
 */
function supportsStreamSaver() {
    return typeof streamSaver !== 'undefined';
}

/**
 * Get the download URL for a file - uses direct B2 URL if available
 */
async function getDownloadURL(fileId) {
    const statusResponse = await fetch(`/api/file/${fileId}/status`);
    handleFetchError(statusResponse);

    const status = await statusResponse.json();
    if (!status.paid) {
        throw new Error('Payment required');
    }

    // Use direct URL if available, otherwise fall back to API
    return {
        url: status.direct_url || `/api/file/${fileId}`,
        isDirect: !!status.direct_url,
        size: status.size
    };
}

/**
 * Download and decrypt using StreamSaver
 */
async function downloadWithStreamSaver(url, totalSize, key, onStatus, isDirect) {
    // Get filename and size from header
    const { filename, fileSize } = await getFileInfoFromRange(url);

    // Create a writable stream using StreamSaver
    const fileStream = streamSaver.createWriteStream(filename, {
        size: fileSize
    });

    const sourceLabel = isDirect ? 'secure storage' : 'server';
    const resultFilename = await decryptWithRangeRequests(url, totalSize, key, fileStream, (progress) => {
        const percent = Math.round(progress * 100);
        const downloaded = Math.round(fileSize * progress);
        onStatus(`Downloading from ${sourceLabel}... ${percent}% (${formatSize(downloaded)} / ${formatSize(fileSize)})`);
    });

    return resultFilename;
}

/**
 * Download and decrypt a file using streaming (for large files)
 * onProgress callback: (phase, progress, message) where phase 0=fetch/decrypt combined for streaming
 */
export async function downloadAndDecryptStreaming(fileId, base64Key, onProgress) {
    onProgress(0, 0, 'Checking file status...');
    const { url, isDirect } = await getDownloadURL(fileId);

    onProgress(0, 0.05, 'Importing decryption key...');
    const key = await importKey(base64Key);

    // Get total encrypted size from HEAD request
    onProgress(0, 0.1, 'Preparing download...');
    const headResponse = await fetch(url, { method: 'HEAD', cache: 'no-store' });
    if (!headResponse.ok) {
        throw new Error('Failed to get file info');
    }
    const totalSize = parseInt(headResponse.headers.get('Content-Length') || '0', 10);

    let resultFilename;

    if (supportsStreamSaver()) {
        // Use StreamSaver - downloads directly to Downloads folder
        const sourceLabel = isDirect ? 'secure storage' : 'server';
        onProgress(0, 0.1, `Downloading from ${sourceLabel}...`);

        // Wrap the status callback to convert to progress callback
        resultFilename = await downloadWithStreamSaver(url, totalSize, key, (status) => {
            // Parse progress from status message if possible
            const match = status.match(/(\d+)%/);
            const progress = match ? parseInt(match[1], 10) / 100 : 0.5;
            onProgress(0, 0.1 + progress * 0.9, status);
        }, isDirect);
    } else {
        throw new Error('Your browser does not support large file downloads. Please use Chrome, Edge, or Firefox.');
    }

    onProgress(0, 1, 'Download complete!');
    return { filename: resultFilename, streamed: true };
}

/**
 * Download and decrypt using in-memory approach (for smaller files)
 * onProgress callback: (phase, progress, message) where phase 0=fetch, 1=decrypt
 */
export async function downloadAndDecrypt(fileId, base64Key, onProgress) {
    onProgress(0, 0, 'Checking file status...');
    const { url, isDirect } = await getDownloadURL(fileId);

    onProgress(0, 0.05, 'Importing decryption key...');
    const key = await importKey(base64Key);

    const sourceLabel = isDirect ? 'secure storage' : 'server';
    onProgress(0, 0.1, `Downloading from ${sourceLabel}...`);

    const response = await fetch(url);
    if (!response.ok) {
        throw new Error('Download failed');
    }

    // Track download progress using ReadableStream
    const contentLength = parseInt(response.headers.get('Content-Length') || '0', 10);

    let encryptedBlob;
    if (contentLength > 0 && response.body) {
        const reader = response.body.getReader();
        const chunks = [];
        let receivedLength = 0;

        while (true) {
            const { done, value } = await reader.read();
            if (done) break;

            chunks.push(value);
            receivedLength += value.length;

            const fetchProgress = 0.1 + (receivedLength / contentLength) * 0.9;
            const percent = Math.round((receivedLength / contentLength) * 100);
            onProgress(0, fetchProgress, `Downloading from ${sourceLabel}... ${percent}% (${formatSize(receivedLength)} / ${formatSize(contentLength)})`);
        }

        encryptedBlob = new Blob(chunks);
    } else {
        // Fallback if Content-Length not available
        encryptedBlob = await response.blob();
    }

    onProgress(1, 0, 'Decrypting file...');

    // Decrypt with progress reporting
    const { blob, filename } = await decryptFile(encryptedBlob, key, (progress) => {
        const percent = Math.round(progress * 100);
        onProgress(1, progress, `Decrypting file... ${percent}%`);
    });

    return { filename, data: blob };
}

/**
 * Smart download that chooses streaming or in-memory based on file size
 * onProgress callback: (phase, progress, message) where phase 0=fetch, 1=decrypt
 */
export async function smartDownload(fileId, base64Key, onProgress) {
    // First, check the file status and get download URL
    onProgress(0, 0, 'Checking file status...');
    const { url, isDirect, size } = await getDownloadURL(fileId);

    // Get exact size from HEAD request
    const headResponse = await fetch(url, { method: 'HEAD', cache: 'no-store' });
    if (!headResponse.ok) {
        throw new Error('Failed to get file info');
    }

    const contentLength = parseInt(headResponse.headers.get('Content-Length') || String(size), 10);

    // Use streaming for large files if StreamSaver is available
    const canStream = supportsStreamSaver();

    if (contentLength > STREAMING_THRESHOLD && canStream) {
        return await downloadAndDecryptStreaming(fileId, base64Key, onProgress);
    }

    // Fall back to in-memory approach for small files
    // For large files without streaming support, this will likely fail
    if (contentLength > STREAMING_THRESHOLD) {
        onProgress(0, 0, 'Warning: Large file download may fail in this browser...');
    }

    return await downloadAndDecrypt(fileId, base64Key, onProgress);
}

function handleFetchError(response) {
    if (response.status === 402) {
        throw new Error('Payment required');
    }
    if (response.status === 410) {
        throw new Error('File has expired');
    }
    if (response.status === 404) {
        throw new Error('File not found');
    }
    if (!response.ok) {
        throw new Error('Download failed');
    }
}

function formatSize(bytes) {
    if (bytes < 1024) return bytes + ' B';
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB';
    if (bytes < 1024 * 1024 * 1024) return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
    return (bytes / (1024 * 1024 * 1024)).toFixed(2) + ' GB';
}

export function triggerDownload(blob, filename) {
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    document.body.removeChild(a);
    URL.revokeObjectURL(url);
}
