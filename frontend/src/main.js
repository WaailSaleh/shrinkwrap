// main.js — ZenithVault Wails frontend (ES module)
'use strict';

import {
  HasMasterPassword,
  HasExistingData,
  CreateMasterPassword,
  UnlockWithPassword,
  IsConfigured,
  GetSettings,
  SaveSettings,
  TestConnection,
  GetVaults,
  StartUpload,
  StartDownload,
  StartDelete,
  OpenFile,
  OpenFilesDialog,
  OpenFolderDialog,
  GenerateGiftToken,
  ImportGiftToken,
} from '../wailsjs/go/main/App.js';

import { EventsOn } from '../wailsjs/runtime/runtime.js';

// ── State ──────────────────────────────────────────────────────────────────────
const state = {
  isUnlocked:      false,
  isConfigured:    false,
  vaults:          [],
  isBusy:          false,
  uploadPaths:     [],
  pendingDeleteID: null,
  giftMode:        'export',
  giftVaultID:     null,
  // Transfer panel
  transferVisible: false,
  smoothedSpeed:   0,   // EMA-smoothed speed in Bps
};

// ── DOM helpers ────────────────────────────────────────────────────────────────
const $ = id => document.getElementById(id);
const show = el => el && el.classList.remove('hidden');
const hide = el => el && el.classList.add('hidden');
const setVisible = (el, v) => v ? show(el) : hide(el);

// ── Transfer panel helpers ─────────────────────────────────────────────────────
const EMA_ALPHA = 0.25; // exponential moving average smoothing factor

function showTransferPanel(op) {
  const panel = $('transfer-panel');
  if (panel.classList.contains('hidden')) {
    panel.classList.remove('hidden');
    $('app').classList.add('has-transfer-panel');
    state.transferVisible = true;
    state.smoothedSpeed   = 0;
  }
  const badge = $('transfer-op-badge');
  const bar   = $('transfer-bar');
  badge.className = 'transfer-panel__op'; // reset modifiers
  bar.className   = 'transfer-panel__bar';
  if (op === 'download') {
    badge.textContent = 'Downloading';
    badge.classList.add('transfer-panel__op--download');
    bar.classList.add('transfer-panel__bar--download');
  } else if (op === 'verify') {
    badge.textContent = 'Verifying';
    badge.classList.add('transfer-panel__op--verify');
    bar.classList.add('transfer-panel__bar--verify');
  } else {
    badge.textContent = 'Uploading';
  }
}

function hideTransferPanel() {
  hide($('transfer-panel'));
  $('app').classList.remove('has-transfer-panel');
  state.transferVisible = false;
  state.smoothedSpeed   = 0;
}

function updateTransferPanel(ev) {
  showTransferPanel(ev.operation);

  // Smooth speed with EMA (skip during verify phase — no active transfer)
  if (ev.operation !== 'verify' && ev.speedBps > 0) {
    state.smoothedSpeed = state.smoothedSpeed === 0
      ? ev.speedBps
      : EMA_ALPHA * ev.speedBps + (1 - EMA_ALPHA) * state.smoothedSpeed;
  }

  // Progress bar
  const pct = Math.min(100, Math.max(0, ev.percentDone || 0));
  $('transfer-bar').style.width = pct + '%';
  $('transfer-pct').textContent  = Math.round(pct) + '%';

  // Filename + chunks
  $('transfer-name').textContent   = ev.filename || '—';
  $('transfer-chunks').textContent =
    ev.chunkTotal > 1 ? `part ${ev.chunkDone}/${ev.chunkTotal}` : '';

  // Bytes
  const bytesLabel = ev.bytesTotal > 0
    ? `${fmtSize(ev.bytesDone)} / ${fmtSize(ev.bytesTotal)}`
    : fmtSize(ev.bytesDone);
  $('transfer-bytes').textContent = bytesLabel;

  // Speed
  $('transfer-speed-label').textContent =
    state.smoothedSpeed > 0 ? fmtSpeed(state.smoothedSpeed) : '—';

  // ETA
  $('transfer-eta').textContent = fmtEta(ev.etaSecs, state.smoothedSpeed);
}

function fmtEta(etaSecs, speedBps) {
  if (!speedBps || speedBps === 0) return '';
  if (etaSecs < 0 || !isFinite(etaSecs)) return '';
  if (etaSecs < 5)  return 'almost done';
  if (etaSecs < 60) return `${Math.round(etaSecs)}s left`;
  const m = Math.floor(etaSecs / 60);
  const s = Math.round(etaSecs % 60);
  return `${m}m ${s}s left`;
}

// ── Network detection ──────────────────────────────────────────────────────────
function updateNetworkStatus() {
  setVisible($('offline-banner'), !navigator.onLine);
}
window.addEventListener('online',  updateNetworkStatus);
window.addEventListener('offline', updateNetworkStatus);

// ── Format helpers ─────────────────────────────────────────────────────────────
function fmtSize(bytes) {
  if (!bytes || bytes === 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(1024));
  return (bytes / Math.pow(1024, i)).toFixed(i > 0 ? 1 : 0) + ' ' + units[i];
}
function fmtSpeed(bps) {
  if (!bps || bps === 0) return '—';
  return (bps / (1024 * 1024)).toFixed(1) + ' MB/s';
}
function fmtDate(iso) {
  if (!iso) return '';
  try {
    return new Date(iso).toLocaleDateString(undefined, {
      year: 'numeric', month: 'short', day: 'numeric',
    });
  } catch { return iso; }
}

function fileTypeBadge(filename) {
  const ext = (filename.split('.').pop() || '').toLowerCase();
  const map = {
    pdf:  ['badge-pdf',   'PDF'],
    png:  ['badge-img',   'IMG'], jpg:  ['badge-img',   'IMG'],
    jpeg: ['badge-img',   'IMG'], gif:  ['badge-img',   'GIF'],
    webp: ['badge-img',   'IMG'], svg:  ['badge-img',   'SVG'],
    zip:  ['badge-zip',   'ZIP'], tar:  ['badge-zip',   'TAR'],
    gz:   ['badge-zip',   'GZ'],  '7z': ['badge-zip',   '7Z'],
    rar:  ['badge-zip',   'RAR'],
    doc:  ['badge-doc',   'DOC'], docx: ['badge-doc',   'DOC'],
    xls:  ['badge-doc',   'XLS'], xlsx: ['badge-doc',   'XLS'],
    ppt:  ['badge-doc',   'PPT'], pptx: ['badge-doc',   'PPT'],
    txt:  ['badge-doc',   'TXT'], md:   ['badge-doc',   'MD'],
    mp4:  ['badge-vid',   'MP4'], mov:  ['badge-vid',   'MOV'],
    mkv:  ['badge-vid',   'MKV'], avi:  ['badge-vid',   'AVI'],
    webm: ['badge-vid',   'WEB'],
    mp3:  ['badge-audio', 'MP3'], wav:  ['badge-audio', 'WAV'],
    flac: ['badge-audio', 'FLA'],
    js:   ['badge-code',  'JS'],  ts:   ['badge-code',  'TS'],
    py:   ['badge-code',  'PY'],  go:   ['badge-code',  'GO'],
    rs:   ['badge-code',  'RS'],  c:    ['badge-code',  'C'],
    cpp:  ['badge-code',  'C++'], sh:   ['badge-code',  'SH'],
    json: ['badge-code',  'JSN'],
  };
  return map[ext] || ['badge-file', ext.toUpperCase().slice(0, 4) || 'FILE'];
}

// ── Safe Go call wrapper ───────────────────────────────────────────────────────
async function goCall(fn, ...args) {
  try {
    return [await fn(...args), null];
  } catch (e) {
    return [null, e?.message || String(e)];
  }
}

// ── Go event listeners ─────────────────────────────────────────────────────────
function setupGoEvents() {
  EventsOn('status', ev => {
    const el = $('status-text');
    el.textContent  = ev.text;
    el.style.color  = ev.color || 'var(--muted)';
  });

  EventsOn('progress', ev => {
    $('progress-text').textContent = ev.text || '';
  });

  EventsOn('speed', ev => {
    $('stat-up-speed').textContent = fmtSpeed(ev.uploadBps);
    $('stat-dl-speed').textContent = fmtSpeed(ev.downloadBps);
  });

  EventsOn('busy', ev => {
    state.isBusy = ev.active;
    setVisible($('busy-strip'), ev.active);
    $('btn-add-vault').disabled = ev.active;
    if (!ev.active) {
      // Operation finished — hide transfer panel after a short delay so the
      // user sees the 100% state before it disappears.
      setTimeout(hideTransferPanel, 1200);
    }
  });

  EventsOn('transfer_progress', ev => {
    updateTransferPanel(ev);
    // Mirror speed into sidebar stats
    if (ev.operation === 'upload') {
      $('stat-up-speed').textContent = state.smoothedSpeed > 0 ? fmtSpeed(state.smoothedSpeed) : '—';
    } else {
      $('stat-dl-speed').textContent = state.smoothedSpeed > 0 ? fmtSpeed(state.smoothedSpeed) : '—';
    }
  });

  EventsOn('vaults_updated', vaults => {
    state.vaults = Array.isArray(vaults) ? vaults : [];
    renderVaultList();
  });

  EventsOn('download_ready', ev => {
    OpenFile(ev.path).catch(() => {});
  });

  EventsOn('error_msg', ev => {
    showError(ev.message);
  });
}

// ── Initialisation ─────────────────────────────────────────────────────────────
window.addEventListener('load', async () => {
  updateNetworkStatus();
  setupGoEvents();
  await init();
});

async function init() {
  const [hasPwd]  = await goCall(HasMasterPassword);
  const [hasData] = await goCall(HasExistingData);

  if (hasPwd) {
    showAuthDialog('unlock');
  } else if (hasData) {
    showAuthDialog('migrate');
  } else {
    showAuthDialog('create');
  }
}

// ── Auth dialog ────────────────────────────────────────────────────────────────
function showAuthDialog(mode) {
  const overlay   = $('auth-overlay');
  const title     = $('auth-title');
  const subtitle  = $('auth-subtitle');
  const submitBtn = $('auth-submit');
  const confirmFld = $('auth-confirm-field');
  const errEl     = $('auth-error');

  hide(errEl);

  if (mode === 'unlock') {
    title.textContent     = 'ZenithVault';
    subtitle.textContent  = 'Enter your master password to unlock.';
    submitBtn.textContent = 'Unlock';
    hide(confirmFld);
  } else if (mode === 'create') {
    title.textContent     = 'Create Master Password';
    subtitle.textContent  = 'Choose a strong password — it encrypts everything locally.';
    submitBtn.textContent = 'Create Vault';
    show(confirmFld);
  } else {
    title.textContent     = 'Set Master Password';
    subtitle.textContent  = 'Existing data found. Set a master password to encrypt credentials.';
    submitBtn.textContent = 'Secure Vault';
    show(confirmFld);
  }

  show(overlay);
  $('auth-password').value = '';
  $('auth-confirm').value  = '';
  $('auth-password').focus();

  // Replace listener cleanly
  submitBtn.onclick = () => handleAuth(mode, overlay, errEl);

  const onKey = e => { if (e.key === 'Enter') { e.preventDefault(); handleAuth(mode, overlay, errEl); } };
  $('auth-password').addEventListener('keydown', onKey);
  $('auth-confirm').addEventListener('keydown', onKey);
}

async function handleAuth(mode, overlay, errEl) {
  hide(errEl);
  const password = $('auth-password').value.trim();
  if (!password) { showDialogError(errEl, 'Password cannot be empty.'); return; }

  const submitBtn = $('auth-submit');
  submitBtn.disabled    = true;
  submitBtn.textContent = mode === 'unlock' ? 'Unlocking…' : 'Setting up…';

  if (mode === 'unlock') {
    const [, err] = await goCall(UnlockWithPassword, password);
    if (err) {
      showDialogError(errEl, 'Incorrect password.');
      submitBtn.disabled    = false;
      submitBtn.textContent = 'Unlock';
      return;
    }
  } else {
    const confirm = $('auth-confirm').value.trim();
    if (password !== confirm) {
      showDialogError(errEl, 'Passwords do not match.');
      submitBtn.disabled    = false;
      submitBtn.textContent = mode === 'create' ? 'Create Vault' : 'Secure Vault';
      return;
    }
    const [, err] = await goCall(CreateMasterPassword, password, mode === 'migrate');
    if (err) {
      showDialogError(errEl, err);
      submitBtn.disabled    = false;
      submitBtn.textContent = mode === 'create' ? 'Create Vault' : 'Secure Vault';
      return;
    }
  }

  state.isUnlocked = true;
  hide(overlay);
  await onUnlocked();
}

async function onUnlocked() {
  show($('app'));
  show($('status-bar'));

  const [configured] = await goCall(IsConfigured);
  state.isConfigured = !!configured;

  const [vaults] = await goCall(GetVaults);
  state.vaults = Array.isArray(vaults) ? vaults : [];
  renderVaultList();

  if (!state.isConfigured) {
    setTimeout(() => showSettings(), 300);
  }
}

// ── Settings dialog ────────────────────────────────────────────────────────────
async function showSettings() {
  const errEl = $('settings-error');
  hide(errEl);

  const [settings] = await goCall(GetSettings);
  if (settings) {
    $('settings-token').value = settings.bot_token || '';
    $('settings-chat').value  = settings.chat_id   || '';
    $('settings-scale').value = settings.ui_scale  || 'Auto';
  }
  show($('settings-overlay'));
  $('settings-token').focus();
}

$('settings-close').onclick = () => hide($('settings-overlay'));
$('btn-settings').onclick   = () => showSettings();

$('settings-test').onclick = async () => {
  $('settings-test').disabled    = true;
  $('settings-test').textContent = 'Testing…';
  const [, err] = await goCall(TestConnection);
  $('settings-test').disabled    = false;
  $('settings-test').textContent = 'Test Connection';
  showDialogError($('settings-error'), err ? '✗ ' + err : '✓ Connection successful',
                  err ? 'var(--danger)' : 'var(--success)');
};

$('settings-save').onclick = async () => {
  const token = $('settings-token').value.trim();
  const chat  = $('settings-chat').value.trim();
  const scale = $('settings-scale').value;
  hide($('settings-error'));

  $('settings-save').disabled = true;
  const [, err] = await goCall(SaveSettings, token, chat, scale);
  $('settings-save').disabled = false;

  if (err) {
    showDialogError($('settings-error'), err);
  } else {
    const [configured] = await goCall(IsConfigured);
    state.isConfigured = !!configured;
    hide($('settings-overlay'));
    setStatus('Settings saved.', 'var(--text)');
  }
};

// ── Upload dialog ──────────────────────────────────────────────────────────────
$('btn-add-vault').onclick = () => {
  if (!state.isConfigured) { showError('Configure Telegram credentials first.'); showSettings(); return; }
  if (state.isBusy) return;
  if (!navigator.onLine) { showError('No network connection — cannot upload while offline.'); return; }
  state.uploadPaths = [];
  $('upload-as-zip').checked           = false;
  $('upload-delete-originals').checked = false;
  renderUploadFileList();
  show($('upload-overlay'));
};

$('upload-close').onclick = () => hide($('upload-overlay'));

$('upload-add-files').onclick = async () => {
  const [paths] = await goCall(OpenFilesDialog);
  if (Array.isArray(paths)) {
    paths.forEach(p => { if (!state.uploadPaths.includes(p)) state.uploadPaths.push(p); });
    renderUploadFileList();
  }
};

$('upload-add-folder').onclick = async () => {
  const [folder] = await goCall(OpenFolderDialog);
  if (folder && !state.uploadPaths.includes(folder)) {
    state.uploadPaths.push(folder);
    renderUploadFileList();
  }
};

$('upload-clear').onclick = () => { state.uploadPaths = []; renderUploadFileList(); };

$('upload-start').onclick = async () => {
  if (state.uploadPaths.length === 0) return;
  const asZip      = $('upload-as-zip').checked;
  const deleteOrig = $('upload-delete-originals').checked;
  hide($('upload-overlay'));
  const [, err] = await goCall(StartUpload, state.uploadPaths, asZip, deleteOrig);
  if (err) showError(err);
};

const dropZone = $('upload-drop-zone');
dropZone.addEventListener('dragover',  e => { e.preventDefault(); dropZone.classList.add('drag-over'); });
dropZone.addEventListener('dragleave', () => dropZone.classList.remove('drag-over'));
dropZone.addEventListener('drop',      e => { e.preventDefault(); dropZone.classList.remove('drag-over'); $('upload-add-files').click(); });

function renderUploadFileList() {
  const ul = $('upload-file-list');
  ul.innerHTML = '';
  state.uploadPaths.forEach(p => {
    const name = p.split(/[/\\]/).pop() || p;
    const li = document.createElement('li');
    li.innerHTML = `<span title="${p}">${name}</span><button class="remove-file">✕</button>`;
    li.querySelector('.remove-file').onclick = () => {
      state.uploadPaths = state.uploadPaths.filter(x => x !== p);
      renderUploadFileList();
    };
    ul.appendChild(li);
  });
  $('upload-start').disabled = state.uploadPaths.length === 0;
}

// ── Vault list ─────────────────────────────────────────────────────────────────
function renderVaultList() {
  const list  = $('vault-list');
  const empty = $('empty-state');
  const n     = state.vaults.length;

  $('stat-count').textContent  = n + ' File' + (n !== 1 ? 's' : '');
  $('count-badge').textContent = n + ' item' + (n !== 1 ? 's' : '');

  const cloudBytes = state.vaults.reduce((s, v) => s + (v.file_size || 0), 0);
  $('stat-cloud').textContent = cloudBytes > 0 ? fmtSize(cloudBytes) : '—';

  list.innerHTML = '';

  if (n === 0) { hide(list); show(empty); return; }
  hide(empty); show(list);

  const BATCH = 10;
  let idx = 0;
  function renderBatch() {
    state.vaults.slice(idx, idx + BATCH).forEach(v => list.appendChild(buildVaultCard(v)));
    idx += BATCH;
    if (idx < state.vaults.length) requestAnimationFrame(renderBatch);
  }
  renderBatch();
}

function buildVaultCard(vault) {
  const [cls, label] = fileTypeBadge(vault.filename);
  const date  = fmtDate(vault.uploaded_at);
  const size  = vault.file_size > 0 ? fmtSize(vault.file_size) : '—';
  const parts = vault.chunk_count > 1 ? ` · ${vault.chunk_count} parts` : '';

  const card = document.createElement('div');
  card.className = 'vault-card';
  card.innerHTML = `
    <div class="vault-card__icon ${cls}">${label}</div>
    <div class="vault-card__body">
      <div class="vault-card__name" title="${vault.filename}">${vault.filename}</div>
      <div class="vault-card__meta">${size}${parts} · ${date}</div>
    </div>
    <div class="vault-card__actions">
      <button class="btn btn--secondary" data-action="gift"   data-id="${vault.id}" title="Gift token">🎁</button>
      <button class="btn btn--secondary" data-action="view"   data-id="${vault.id}">View</button>
      <button class="btn btn--danger"    data-action="delete" data-id="${vault.id}">Delete</button>
    </div>`;

  card.querySelectorAll('button[data-action]').forEach(btn => {
    btn.addEventListener('click', e => {
      e.stopPropagation();
      const id = parseInt(btn.dataset.id, 10);
      if (btn.dataset.action === 'view')   handleView(id);
      if (btn.dataset.action === 'delete') handleDelete(id, vault.filename, vault.chunk_count);
      if (btn.dataset.action === 'gift')   handleGiftExport(id);
    });
  });
  return card;
}

// ── View ───────────────────────────────────────────────────────────────────────
async function handleView(vaultID) {
  if (!state.isConfigured) { showError('Telegram not configured.'); return; }
  if (state.isBusy)        { showError('Another operation is in progress.'); return; }
  if (!navigator.onLine)   { showError('No network connection — cannot download while offline.'); return; }
  const [, err] = await goCall(StartDownload, vaultID);
  if (err) showError(err);
}

// ── Delete ─────────────────────────────────────────────────────────────────────
function handleDelete(vaultID, filename, chunkCount) {
  $('confirm-title').textContent   = 'Confirm Delete';
  $('confirm-message').textContent =
    `Permanently delete "${filename}" (${chunkCount} Telegram part${chunkCount !== 1 ? 's' : ''}) from the vault?\n\nThis cannot be undone.`;
  state.pendingDeleteID = vaultID;
  show($('confirm-overlay'));
}

$('confirm-cancel').onclick = () => { hide($('confirm-overlay')); state.pendingDeleteID = null; };

$('confirm-ok').onclick = async () => {
  hide($('confirm-overlay'));
  if (state.pendingDeleteID === null) return;
  const id = state.pendingDeleteID;
  state.pendingDeleteID = null;
  const [, err] = await goCall(StartDelete, id);
  if (err) showError(err);
};

// ── Gift tokens ────────────────────────────────────────────────────────────────
async function handleGiftExport(vaultID) {
  $('gift-title').textContent    = 'Gift Token';
  $('gift-subtitle').textContent = 'Share this token with someone who has ZenithVault and Telegram access.';
  $('gift-token-area').value     = 'Generating…';
  show($('gift-token-area'));
  hide($('gift-import-fields'));
  show($('gift-copy'));
  hide($('gift-import-btn'));
  hide($('gift-error'));
  state.giftMode = 'export';
  show($('gift-overlay'));

  const [token, err] = await goCall(GenerateGiftToken, vaultID);
  if (err) {
    $('gift-token-area').value = '';
    showDialogError($('gift-error'), err);
  } else {
    $('gift-token-area').value = token;
  }
}

$('btn-import-gift').onclick = () => {
  $('gift-title').textContent    = 'Import Gift Token';
  $('gift-subtitle').textContent = 'Paste a gift token to import a shared vault entry.';
  hide($('gift-token-area'));
  show($('gift-import-fields'));
  hide($('gift-copy'));
  show($('gift-import-btn'));
  hide($('gift-error'));
  $('gift-import-token').value    = '';
  $('gift-import-filename').value = '';
  state.giftMode = 'import';
  show($('gift-overlay'));
};

$('gift-close').onclick = () => hide($('gift-overlay'));

$('gift-copy').onclick = () => {
  navigator.clipboard.writeText($('gift-token-area').value).catch(() => {});
  $('gift-copy').textContent = 'Copied!';
  setTimeout(() => { $('gift-copy').textContent = 'Copy Token'; }, 1500);
};

$('gift-import-btn').onclick = async () => {
  const token    = $('gift-import-token').value.trim();
  const filename = $('gift-import-filename').value.trim() || 'imported_gift';
  if (!token) { showDialogError($('gift-error'), 'Paste a gift token first.'); return; }

  $('gift-import-btn').disabled    = true;
  $('gift-import-btn').textContent = 'Importing…';
  const [, err] = await goCall(ImportGiftToken, token, filename);
  $('gift-import-btn').disabled    = false;
  $('gift-import-btn').textContent = 'Import';

  if (err) showDialogError($('gift-error'), err);
  else     hide($('gift-overlay'));
};

// ── Sidebar toggle ─────────────────────────────────────────────────────────────
let sidebarVisible = true;
$('sidebar-toggle').addEventListener('click', () => {
  sidebarVisible = !sidebarVisible;
  $('sidebar').classList.toggle('collapsed', !sidebarVisible);
  $('sidebar-toggle').textContent = sidebarVisible ? '◀' : '▶';
});

window.addEventListener('resize', () => {
  if (window.innerWidth < 720 && sidebarVisible) {
    sidebarVisible = false;
    $('sidebar').classList.add('collapsed');
    $('sidebar-toggle').textContent = '▶';
  } else if (window.innerWidth >= 720 && !sidebarVisible) {
    sidebarVisible = true;
    $('sidebar').classList.remove('collapsed');
    $('sidebar-toggle').textContent = '◀';
  }
});

// ── Status helpers ─────────────────────────────────────────────────────────────
function setStatus(text, color) {
  $('status-text').textContent = text;
  $('status-text').style.color = color || '';
}

function showError(msg) {
  setStatus(msg, 'var(--danger)');
  setTimeout(() => { $('status-text').style.color = ''; }, 5000);
}

function showDialogError(el, msg, color) {
  el.textContent = msg;
  el.style.color = color || 'var(--danger)';
  show(el);
}
