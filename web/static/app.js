// Entry point of the UI: wires the views together, switches tabs and runs
// the one poller that keeps backup and restore progress (and the backend-down
// page) up to date.

import { api, onBackendStatus } from "./api.js";
import { createBackupView } from "./backup.js";
import { createHistoryView } from "./history.js";
import { createProgressView } from "./progress.js";
import { createRestoreView } from "./restore.js";
import { createRestoresView, openRestoreDialog } from "./restores.js";
import { createWizard } from "./wizard.js";

const POLL_ACTIVE_MS = 500;
const POLL_IDLE_MS = 3000;
const POLL_DOWN_MS = 2000;

const tabs = {
  wizard: { tab: document.getElementById("tab-wizard"), view: document.getElementById("view-wizard") },
  progress: { tab: document.getElementById("tab-progress"), view: document.getElementById("view-progress") },
  history: { tab: document.getElementById("tab-history"), view: document.getElementById("view-history") },
  backup: { tab: document.getElementById("tab-backup"), view: document.getElementById("view-backup") },
  restores: { tab: document.getElementById("tab-restores"), view: document.getElementById("view-restores") },
  restore: { tab: document.getElementById("tab-restore"), view: document.getElementById("view-restore") },
};
const liveDot = document.getElementById("tab-progress-dot");
const restoreDot = document.getElementById("tab-restores-dot");

let currentTab = "wizard";
let activeBackupId = null; // id of the running/paused backup, if any
let activeRestoreId = null; // id of the running/paused restore, if any
let pollTimer = 0;
let backendUp = true;

// "Backend not available" page: shown while requests fail, hidden again as
// soon as any request gets an answer. The poller keeps retrying meanwhile.
const backendDownEl = document.getElementById("backend-down");
const backendStatusEl = document.getElementById("backend-down-status");
document.getElementById("backend-origin").textContent = location.origin;
document.getElementById("backend-retry").addEventListener("click", () => {
  backendStatusEl.textContent = "Retrying…";
  poll();
});

onBackendStatus((up) => {
  backendUp = up;
  backendDownEl.hidden = up;
  if (up) {
    // Catch up on whatever changed while the backend was away.
    if (currentTab === "history") refreshHistory();
  } else {
    document.getElementById("backend-retry").focus();
  }
});

const wizard = createWizard({
  onBackupStarted(backup) {
    activeBackupId = backup.id;
    progress.show(backup);
    selectTab("progress");
    poll();
  },
});

const backupView = createBackupView({
  async onRestore(backupId) {
    const restore = await openRestoreDialog(backupId);
    if (!restore) return;
    activeRestoreId = restore.id;
    restoresView.show(restore);
    selectTab("restores");
    poll();
  },
});

const restoreView = createRestoreView();

const restoresView = createRestoresView({
  onBrowse(restore) {
    restoreView.open(restore, Number(document.getElementById("page-size").value));
    tabs.restore.tab.hidden = false;
    selectTab("restore");
  },
});

function browseBackup(backup) {
  backupView.open(backup, Number(document.getElementById("page-size").value));
  tabs.backup.tab.hidden = false;
  selectTab("backup");
}

const progress = createProgressView({ onBrowse: browseBackup });

const history = createHistoryView({
  onBrowse: browseBackup,
  async onShowPaths(backup) {
    selectTab("wizard");
    await wizard.showPaths(backup.roots);
  },
  onShowProgress(backup) {
    progress.show(backup);
    selectTab("progress");
  },
  // Close whatever still shows the deleted backup or one of its restores.
  async onDeleted(backup) {
    if (backupView.backupId === backup.id) {
      backupView.close();
      tabs.backup.tab.hidden = true;
    }
    if (progress.shownId === backup.id) progress.show(null);
    const remaining = await api.restores.list().catch(() => null);
    if (remaining) {
      const exists = (id) => remaining.some((c) => c.id === id);
      if (restoreView.restoreId != null && !exists(restoreView.restoreId)) {
        restoreView.close();
        tabs.restore.tab.hidden = true;
      }
      if (restoresView.shownId != null && !exists(restoresView.shownId)) restoresView.show(remaining[0] ?? null);
    }
  },
});

async function selectTab(name) {
  currentTab = name;
  for (const [key, { tab, view }] of Object.entries(tabs)) {
    tab.setAttribute("aria-selected", String(key === name));
    view.hidden = key !== name;
  }
  if (name === "wizard") wizard.focus();
  if (name === "backup") backupView.focus();
  if (name === "restore") restoreView.focus();
  if (name === "restores") {
    await refreshRestores();
    if (activeRestoreId == null && restoresView.shownId == null) {
      const list = await api.restores.list().catch(() => []);
      restoresView.show(list[0] ?? null);
    }
  }
  if (name === "history") await refreshHistory();
  if (name === "progress" && activeBackupId == null && progress.shownId == null) {
    // Nothing active: show the most recent backup, if there is one.
    const backups = await api.backups.list().catch(() => []);
    progress.show(backups[0] ?? null);
  }
}

async function refreshRestores() {
  try {
    restoresView.renderHistory(await api.restores.list());
  } catch {
    // Keep the last rendering; the next poll retries.
  }
}

async function refreshHistory() {
  try {
    history.render(await api.backups.list());
  } catch {
    // Keep the last rendering; the next poll retries.
  }
}

/** Tracks the active backup and restore; fast while one runs, slow otherwise. */
async function poll() {
  clearTimeout(pollTimer);
  try {
    const current = await api.backups.current();
    if (current) {
      activeBackupId = current.id;
      if (progress.shownId == null || progress.shownId === current.id) progress.show(current);
    } else if (activeBackupId != null) {
      // The backup just ended: show its stored final result.
      const finished = await api.backups.get(activeBackupId);
      if (progress.shownId === activeBackupId) progress.show(finished);
      activeBackupId = null;
    }
    wizard.setBackupActive(current != null);
    liveDot.hidden = current == null;
    if (currentTab === "history") await refreshHistory();

    // The running or paused restore, like the backup above.
    const restore = await api.restores.current();
    if (restore) {
      activeRestoreId = restore.id;
      if (restoresView.shownId == null || restoresView.shownId === restore.id) restoresView.show(restore);
    } else if (activeRestoreId != null) {
      const finished = await api.restores.get(activeRestoreId);
      if (restoresView.shownId === activeRestoreId) restoresView.show(finished);
      activeRestoreId = null;
    }
    restoreDot.hidden = restore == null;
    if (currentTab === "restores") await refreshRestores();
  } catch (err) {
    // Unreachable: api.js has switched to the "backend not available" page.
    if (!backendUp) backendStatusEl.textContent = `${err.message}. Last attempt ${new Date().toLocaleTimeString()}, retrying every ${POLL_DOWN_MS / 1000} s…`;
  }
  const active = activeBackupId != null || activeRestoreId != null;
  pollTimer = setTimeout(poll, !backendUp ? POLL_DOWN_MS : active ? POLL_ACTIVE_MS : POLL_IDLE_MS);
}

for (const [name, { tab }] of Object.entries(tabs)) {
  tab.addEventListener("click", () => selectTab(name));
}

// Ctrl+Enter: show the selected items in the wizard tree.
document.addEventListener("keydown", (e) => {
  if (currentTab === "wizard" && e.key === "Enter" && (e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey) {
    e.preventDefault();
    wizard.revealSelected();
  }
});

poll();
wizard.focus();
