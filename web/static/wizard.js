// "New backup" view: pick paths in the file tree and start a backup.

import { api } from "./api.js";
import { formatBytesExact, formatCount } from "./format.js";
import { FileTree } from "./tree.js";

const PAGE_SIZE_KEY = "data_organizer.pageSize";

const treeEl = document.getElementById("tree");
const pageSizeEl = document.getElementById("page-size");
const selectionEl = document.getElementById("selection");
const countEl = document.getElementById("selection-count");
const revealEl = document.getElementById("reveal-selected");
const revealLabelEl = revealEl.querySelector(".button-label");
const clearEl = document.getElementById("clear-selection");
const startEl = document.getElementById("start-backup");

function storedPageSize() {
  try {
    const value = Number(localStorage.getItem(PAGE_SIZE_KEY));
    return [...pageSizeEl.options].some((o) => Number(o.value) === value) ? value : null;
  } catch {
    return null;
  }
}

function describe(row) {
  if (!row) return "";
  const { node } = row;
  if (row.type !== "node") {
    return node.entry ? node.entry.path : "";
  }
  const { entry } = node;
  if (entry.kind === "file") return `${entry.path} · ${formatBytesExact(entry.size)}`;
  if (node.loaded) {
    const shown = node.children.length;
    const items = `${formatCount(node.total)} item${node.total === 1 ? "" : "s"}`;
    return shown < node.total ? `${entry.path} · ${items}, ${formatCount(shown)} loaded` : `${entry.path} · ${items}`;
  }
  return entry.path;
}

/**
 * @param {object} hooks { onBackupStarted(backup) }
 */
export function createWizard({ onBackupStarted }) {
  let revealing = false;
  let backupActive = false;
  let starting = false;

  const initialPageSize = storedPageSize();
  if (initialPageSize) pageSizeEl.value = String(initialPageSize);

  const tree = new FileTree(treeEl, api, {
    pageSize: Number(pageSizeEl.value),
    onActiveChange: (row) => {
      selectionEl.textContent = describe(row);
    },
    onSelectionChange: () => {
      countEl.title = "";
      showSelectionCount();
    },
  });

  function showSelectionCount() {
    const n = tree.selection.size;
    countEl.classList.remove("notice");
    countEl.textContent = n === 0 ? "No selection" : `${formatCount(n)} selected`;
    revealEl.disabled = n === 0 || revealing;
    clearEl.disabled = n === 0;
    updateStartButton();
  }

  function updateStartButton() {
    startEl.disabled = tree.selection.size === 0 || backupActive || starting;
    startEl.textContent = backupActive ? "Backup in progress" : starting ? "Starting…" : "Start backup";
    startEl.title = backupActive ? "Only one backup can run at a time; stop or finish the current one first" : "";
  }

  function notice(text, title = "") {
    countEl.classList.add("notice");
    countEl.textContent = text;
    countEl.title = title;
  }

  async function revealSelected() {
    if (revealing || tree.selection.size === 0) return;
    revealing = true;
    revealEl.disabled = true;
    revealLabelEl.textContent = "Showing…";
    try {
      const missing = await tree.revealSelected();
      showSelectionCount();
      if (missing.length > 0) {
        notice(`${formatCount(missing.length)} of ${formatCount(tree.selection.size)} selected not found`, missing.join("\n"));
      }
    } finally {
      revealing = false;
      revealLabelEl.textContent = "Show selected";
      revealEl.disabled = tree.selection.size === 0;
      treeEl.focus();
    }
  }

  async function startBackup() {
    if (startEl.disabled) return;
    starting = true;
    updateStartButton();
    try {
      const backup = await api.backups.start([...tree.selection.keys()]);
      backupActive = true;
      onBackupStarted(backup);
    } catch (err) {
      notice(`Could not start backup: ${err.message}`);
    } finally {
      starting = false;
      updateStartButton();
    }
  }

  pageSizeEl.addEventListener("change", () => {
    tree.pageSize = Number(pageSizeEl.value);
    try {
      localStorage.setItem(PAGE_SIZE_KEY, pageSizeEl.value);
    } catch {
      // Storage unavailable; the choice just won't persist.
    }
    tree.render();
    treeEl.focus();
  });

  document.getElementById("collapse-all").addEventListener("click", () => {
    tree.collapseAll();
    treeEl.focus();
  });

  revealEl.addEventListener("click", revealSelected);
  clearEl.addEventListener("click", () => {
    tree.clearSelection();
    treeEl.focus();
  });
  startEl.addEventListener("click", startBackup);

  tree.start();

  return {
    focus: () => treeEl.focus(),
    revealSelected,
    /** Called by the app whenever it learns whether a backup is active. */
    setBackupActive(active) {
      backupActive = active;
      updateStartButton();
    },
    /** Shows the given paths selected in the tree and expands to them. */
    async showPaths(paths) {
      tree.setSelection(paths);
      await revealSelected();
    },
  };
}
