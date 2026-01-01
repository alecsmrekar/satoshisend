import { uploadFile, pollPaymentStatus } from './upload.js';
import { smartDownload, triggerDownload } from './download.js';

// DOM elements
const uploadSection = document.getElementById('upload-section');
const paymentSection = document.getElementById('payment-section');
const shareSection = document.getElementById('share-section');
const downloadSection = document.getElementById('download-section');

const fileInput = document.getElementById('file-input');
const uploadBtn = document.getElementById('upload-btn');
const uploadProgress = document.getElementById('upload-progress');
const uploadStatus = document.getElementById('upload-status');
const dropZone = document.getElementById('drop-zone');
const selectedFileName = document.getElementById('selected-file-name');

// Phase progress helper functions
function updatePhase(container, phaseIndex, progress, isComplete = false) {
    const phases = container.querySelectorAll('.phase');

    phases.forEach((phase, i) => {
        const fill = phase.querySelector('.phase-fill');

        if (i < phaseIndex) {
            // Previous phases are complete
            phase.classList.remove('active');
            phase.classList.add('complete');
            fill.style.width = '100%';
        } else if (i === phaseIndex) {
            // Current phase
            phase.classList.add('active');
            phase.classList.toggle('complete', isComplete);
            fill.style.width = `${Math.round(progress * 100)}%`;
        } else {
            // Future phases
            phase.classList.remove('active', 'complete');
            fill.style.width = '0%';
        }
    });
}

function resetPhases(container) {
    const phases = container.querySelectorAll('.phase');
    phases.forEach(phase => {
        phase.classList.remove('active', 'complete');
        phase.querySelector('.phase-fill').style.width = '0%';
    });
}

function completeAllPhases(container) {
    const phases = container.querySelectorAll('.phase');
    phases.forEach(phase => {
        phase.classList.remove('active');
        phase.classList.add('complete');
        phase.querySelector('.phase-fill').style.width = '100%';
    });
}

const invoiceCode = document.getElementById('invoice-code');
const copyInvoiceBtn = document.getElementById('copy-invoice');
const amountSats = document.getElementById('amount-sats');
const paymentStatus = document.getElementById('payment-status');
const qrContainer = document.getElementById('qr-container');

const shareUrl = document.getElementById('share-url');
const copyLinkBtn = document.getElementById('copy-link');

const downloadIntro = document.getElementById('download-intro');
const startDownloadBtn = document.getElementById('start-download-btn');
const downloadProgressEl = document.getElementById('download-progress');
const downloadStatus = document.getElementById('download-status');
const saveFileBtn = document.getElementById('save-file-btn');

// Route based on URL
const pathParts = window.location.pathname.split('/');
const hash = window.location.hash.slice(1); // Remove the #

if (pathParts[1] === 'file' && pathParts[2] && hash) {
    // Download mode: /file/{id}#{key}
    initDownload(pathParts[2], hash);
} else if (pathParts[1] === 'pending' && pathParts[2] && hash) {
    // Pending payment mode: /pending/{id}#{key}
    initPending(pathParts[2], hash);
} else {
    // Upload mode
    initUpload();
}

function initUpload() {
    uploadSection.classList.remove('hidden');

    // File input change handler
    fileInput.addEventListener('change', () => {
        handleFileSelection(fileInput.files[0]);
    });

    // Click to upload
    dropZone.addEventListener('click', () => {
        fileInput.click();
    });

    // Drag and drop handlers
    dropZone.addEventListener('dragover', (e) => {
        e.preventDefault();
        dropZone.classList.add('dragover');
    });

    dropZone.addEventListener('dragleave', (e) => {
        e.preventDefault();
        dropZone.classList.remove('dragover');
    });

    dropZone.addEventListener('drop', (e) => {
        e.preventDefault();
        dropZone.classList.remove('dragover');

        const files = e.dataTransfer.files;
        if (files.length > 0) {
            fileInput.files = files;
            handleFileSelection(files[0]);
        }
    });

    // Upload button click
    uploadBtn.addEventListener('click', async () => {
        const file = fileInput.files[0];
        if (!file) return;

        uploadBtn.disabled = true;
        uploadProgress.classList.remove('hidden');
        resetPhases(uploadProgress);
        uploadStatus.textContent = '';
        uploadStatus.classList.remove('error');

        try {
            const result = await uploadFile(file, (phase, progress, message) => {
                updatePhase(uploadProgress, phase, progress);
                uploadStatus.textContent = message;
            });

            completeAllPhases(uploadProgress);
            uploadStatus.textContent = 'Redirecting to payment...';

            // Small delay to show completion
            await new Promise(r => setTimeout(r, 500));

            // Redirect to pending page (preserves state on refresh)
            window.location.href = `/pending/${result.fileId}#${result.key}`;

        } catch (err) {
            uploadStatus.textContent = 'Error: ' + err.message;
            uploadStatus.classList.add('error');
            uploadBtn.disabled = false;
        }
    });

    copyInvoiceBtn.addEventListener('click', () => copyToClipboard(copyInvoiceBtn, invoiceCode.textContent));
    copyLinkBtn.addEventListener('click', () => {
        shareUrl.select();
        copyToClipboard(copyLinkBtn, shareUrl.value);
    });
}

function truncateFilename(name, maxLength = 40) {
    if (name.length <= maxLength) return name;

    const extIndex = name.lastIndexOf('.');
    const ext = extIndex > 0 ? name.slice(extIndex) : '';
    const baseName = extIndex > 0 ? name.slice(0, extIndex) : name;

    const availableLength = maxLength - ext.length - 3; // 3 for "..."
    const frontLength = Math.ceil(availableLength / 2);
    const backLength = Math.floor(availableLength / 2);

    return baseName.slice(0, frontLength) + '...' + baseName.slice(-backLength) + ext;
}

function handleFileSelection(file) {
    if (!file) return;

    uploadBtn.disabled = false;
    dropZone.classList.add('has-file');
    selectedFileName.textContent = truncateFilename(file.name);
    selectedFileName.classList.remove('hidden');
}

function copyToClipboard(button, text) {
    navigator.clipboard.writeText(text);
    const originalText = button.textContent;
    button.textContent = 'Copied!';
    button.classList.add('copy-success');
    setTimeout(() => {
        button.textContent = originalText;
        button.classList.remove('copy-success');
    }, 2000);
}

async function generateQRCode(text) {
    if (typeof QRCode !== 'undefined' && qrContainer) {
        qrContainer.innerHTML = '';
        try {
            const canvas = document.createElement('canvas');
            await QRCode.toCanvas(canvas, text.toUpperCase(), {
                width: 200,
                margin: 2,
                color: {
                    dark: '#000000',
                    light: '#ffffff'
                }
            });
            qrContainer.appendChild(canvas);
        } catch (err) {
            console.error('QR code generation failed:', err);
        }
    }
}

async function initPending(fileId, key) {
    uploadSection.classList.add('hidden');
    paymentSection.classList.remove('hidden');

    try {
        // Check if already paid
        const statusResp = await fetch(`/api/file/${fileId}/status`);
        if (statusResp.ok) {
            const status = await statusResp.json();
            if (status.paid) {
                // Already paid, go to share page
                showSharePage(fileId, key);
                return;
            }
        }

        // Fetch the invoice
        const invoiceResp = await fetch(`/api/file/${fileId}/invoice`);
        if (!invoiceResp.ok) {
            throw new Error('Could not fetch invoice. File may have expired.');
        }

        const invoice = await invoiceResp.json();
        invoiceCode.textContent = invoice.payment_request;
        amountSats.textContent = invoice.amount_sats;

        // Generate QR code
        await generateQRCode(invoice.payment_request);

        // Setup copy button
        copyInvoiceBtn.addEventListener('click', () => copyToClipboard(copyInvoiceBtn, invoiceCode.textContent));

        // Poll for payment
        pollPaymentStatus(fileId, () => {
            updatePaymentStatus('Payment received!', true);
            setTimeout(() => showSharePage(fileId, key), 1000);
        });

    } catch (err) {
        paymentStatus.innerHTML = `<span style="color: var(--error-red)">Error: ${err.message}</span>`;
    }
}

function updatePaymentStatus(message, success = false) {
    paymentStatus.innerHTML = success
        ? `<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M20 6L9 17l-5-5"/></svg><span>${message}</span>`
        : `<div class="spinner"></div><span>${message}</span>`;

    if (success) {
        paymentStatus.classList.add('success');
    }
}

function showSharePage(fileId, key) {
    paymentSection.classList.add('hidden');
    shareSection.classList.remove('hidden');

    const url = `${window.location.origin}/file/${fileId}#${key}`;
    shareUrl.value = url;

    copyLinkBtn.addEventListener('click', () => {
        shareUrl.select();
        copyToClipboard(copyLinkBtn, shareUrl.value);
    });
}

function initDownload(fileId, key) {
    uploadSection.classList.add('hidden');
    downloadSection.classList.remove('hidden');

    // Set up the download button click handler
    startDownloadBtn.addEventListener('click', async () => {
        // Hide intro, show progress
        downloadIntro.classList.add('hidden');
        startDownloadBtn.classList.add('hidden');
        downloadProgressEl.classList.remove('hidden');

        resetPhases(downloadProgressEl);
        downloadStatus.textContent = '';
        downloadStatus.classList.remove('error');

        try {
            const result = await smartDownload(fileId, key, (phase, progress, message) => {
                updatePhase(downloadProgressEl, phase, progress);
                downloadStatus.textContent = message;
            });

            completeAllPhases(downloadProgressEl);

            if (result.streamed) {
                downloadStatus.textContent = `Saved: ${result.filename}`;
            } else {
                const { filename, data } = result;
                downloadStatus.textContent = `Ready: ${filename}`;

                saveFileBtn.classList.remove('hidden');
                saveFileBtn.addEventListener('click', () => {
                    triggerDownload(data, filename);
                });

                // Auto-trigger browser download after decryption
                triggerDownload(data, filename);
            }

        } catch (err) {
            downloadStatus.textContent = 'Error: ' + err.message;
            downloadStatus.classList.add('error');
        }
    });
}
