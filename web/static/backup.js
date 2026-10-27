// "Backup" view: browse a completed backup and mark items as excluded.
// Same tree as the wizard, but only backed-up items, no selection and no way
// to start a backup. Later copies of duplicates are struck through.

import { api } from "./api.js";
import { formatBytesExact, formatCount } from "./format.js";
import { confirmDialog } from "./confirm.js";
import { FileTree } from "./tree.js";

const $ = (id) => document.getElementById(id);

const STATE_CLASSES = ["not-first", "unreadable", "excluded"];

function joinPath(dir, name) {
  const sep = dir.includes("\\") ? "\\" : "/";
  return dir.endsWith(sep) ? dir + name : dir + sep + name;
}

/** Adapts the backup entries API to the tree's data source interface. */
function entrySource(backupId) {
  return {
    roots: () => api.backups.entryRoots(backupId).then((entries) => entries.map((e) => ({ ...e, path: e.name }))),
    list: (path, offset, limit, parent) =>
      api.backups.entryChildren(backupId, parent.id, offset, limit).then((page) => ({
        ...page,
        entries: page.entries.map((e) => ({ ...e, path: joinPath(path, e.name) })),
      })),
  };
}

// Everything is yellow; a later copy (an earlier copy by path exists that
// is not excluded) is struck through; excluded items are faded.
function decorateRow(el, entry) {
  el.classList.remove(...STATE_CLASSES);
  if (entry.error) el.classList.add("unreadable");
  if (entry.notFirst) el.classList.add("not-first");
  if (entry.excluded) el.classList.add("excluded");
  el.title = entry.error ? `Unreadable: ${entry.error}` : "";
}

function describe(row) {
  if (!row) return "";
  if (row.type !== "node") return row.node.entry?.path ?? "";
  const { entry } = row.node;
  const parts = [entry.path];
  if (entry.size != null) parts.push(formatBytesExact(entry.size));
  if (entry.error) {
    parts.push(`unreadable: ${entry.error}`);
  } else {
    if (entry.dupCount === 0) parts.push("unique");
    else parts.push(`${formatCount(entry.dupCount)} copies${entry.notFirst ? ", an earlier copy exists" : ""}`);
    if (entry.excludedSelf) parts.push("excluded");
    else if (entry.excluded) parts.push("excluded with its folder");
  }
  return parts.join(" · ");
}

/**
 * @param {object} hooks { onRestore(backupId) }
 */
export function createBackupView({ onRestore }) {
  const containerEl = $("backup-tree-container");
  const statusEl = $("backup-selection");
  const noticeEl = $("backup-notice");
  const excludeEl = $("backup-exclude");

  let tree = null;
  let treeEl = null;
  let backupId = null;
  let busy = false;

  /** Entries of the focused range (or the cursor row) that can be marked. */
  const excludable = () => (tree?.rangeRows() ?? []).map((r) => r.node.entry).filter((e) => !e.error);

  // Toggle direction: Include only when every item is explicitly excluded
  // (items excluded through a folder are included by including the folder).
  const allExcluded = (entries) => entries.length > 0 && entries.every((e) => e.excludedSelf);

  function updateButtons() {
    const ex = excludable();
    excludeEl.disabled = busy || ex.length === 0;
    excludeEl.querySelector(".button-label").textContent = allExcluded(ex) ? "Include" : "Exclude";
    $("backup-reset").disabled = busy;
    $("backup-restore").disabled = busy;
  }

  function notice(text, isError = false) {
    noticeEl.textContent = text;
    noticeEl.hidden = !text;
    noticeEl.classList.toggle("notice", isError); // red only for errors
  }


  /**
   * One exclusion can change many loaded items. All of them are marked
   * stale; only the rendered ones are fetched now, the others when they
   * scroll into view (refreshStale via onRowsRendered).
   */
  async function refreshState() {
    for (const e of tree.loadedEntries()) e.stale = true;
    await refreshStale(tree.renderedEntries());
  }

  let refreshing = null;
  async function refreshStale(entries) {
    const stale = entries.filter((e) => e.stale);
    if (stale.length === 0) return;
    for (const e of stale) e.stale = false; // a concurrent call skips them
    const byId = new Map(stale.map((e) => [e.id, e]));
    const backup = backupId;
    const fresh = await api.backups.state(backup, [...byId.keys()]);
    if (backup !== backupId) return; // another backup was opened meanwhile
    for (const f of fresh) Object.assign(byId.get(f.id), f); // keeps the client-side path
    tree.redecorate();
  }
  const onRowsRendered = (entries) => {
    if (!entries.some((e) => e.stale)) return;
    refreshing = (refreshing ?? Promise.resolve()).then(() => refreshStale(entries)).catch(() => {});
  };

  // Exclude (Delete): if every item in the range is explicitly excluded,
  // include them all, otherwise exclude them all, including everything
  // below folders.
  async function exclude() {
    const entries = excludable();
    if (!tree || busy || entries.length === 0) return;
    const on = !allExcluded(entries);
    busy = true;
    updateButtons();
    notice("Applying…");
    try {
      // Only the marks change on the server; the subtrees follow when read.
      await api.backups.exclude(backupId, entries.map((e) => e.id), on);
      await refreshState();
      notice("");
    } catch (err) {
      notice(`Could not ${on ? "exclude" : "include"}: ${err.message}`, true);
    } finally {
      busy = false;
      updateButtons();
      treeEl.focus();
    }
  }

  function onKey(e, row) {
    if (row?.type === "node" && e.key === "Delete") {
      exclude();
      return true;
    }
    return false;
  }

  async function resetMarks() {
    if (!tree || busy) return;
    const ok = await confirmDialog({
      title: `Reset exclusions of backup #${backupId}?`,
      text: "This removes every exclusion in this backup. It cannot be undone.",
      okLabel: "Reset exclusions",
    });
    if (!ok) return;
    busy = true;
    updateButtons();
    try {
      const { removed } = await api.backups.reset(backupId);
      await refreshState();
      notice(removed === 0 ? "There were no exclusions to reset." : `Reset: removed ${formatCount(removed)} exclusion${removed === 1 ? "" : "s"}.`);
    } catch (err) {
      notice(`Could not reset: ${err.message}`, true);
    } finally {
      busy = false;
      updateButtons();
      treeEl.focus();
    }
  }

  $("backup-reset").addEventListener("click", resetMarks);
  $("backup-restore").addEventListener("click", () => backupId && onRestore(backupId));
  excludeEl.addEventListener("click", exclude);
  $("backup-collapse-all").addEventListener("click", () => {
    tree?.collapseAll();
    treeEl?.focus();
  });

  return {
    /** Opens a completed backup (keeps state if already open). */
    open(backup, pageSize) {
      $("backup-title").textContent = `Backup #${backup.id}`;
      if (backupId === backup.id) return;
      backupId = backup.id;
      notice("");
      // A fresh element per backup: the tree binds its listeners to it.
      treeEl = document.createElement("div");
      treeEl.className = "tree";
      treeEl.tabIndex = 0;
      treeEl.setAttribute("role", "tree");
      treeEl.setAttribute("aria-label", `Backup #${backup.id}`);
      containerEl.replaceChildren(treeEl);
      tree = new FileTree(treeEl, entrySource(backup.id), {
        pageSize,
        selectable: false,
        decorateRow,
        onRowsRendered,
        onKey,
        onActiveChange: (row) => {
          const range = tree?.rangeRows().length ?? 0;
          statusEl.textContent = range > 1 ? `${formatCount(range)} items in range · ${describe(row)}` : describe(row);
          updateButtons();
        },
      });
      tree.start();
    },
    /** Forgets the open backup (it was deleted). */
    close() {
      backupId = null;
      tree = treeEl = null;
      containerEl.replaceChildren();
    },
    get backupId() {
      return backupId;
    },
    focus: () => treeEl?.focus(),
  };
}
