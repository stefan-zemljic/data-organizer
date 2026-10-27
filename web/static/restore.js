// "Restore" view: browse what a restore did. Files: restored, or failed
// (red: not written, or the stored content is damaged). Folders show a red
// dot when something below failed.

import { api } from "./api.js";
import { formatCount } from "./format.js";
import { FileTree } from "./tree.js";

const $ = (id) => document.getElementById(id);

const CLASSES = ["restore-failed", "below-errors"];

const OUTCOME_TEXT = {
  ok: "restored",
  failed: "failed",
};

function joinPath(dir, name) {
  const sep = dir.includes("\\") ? "\\" : "/";
  return dir.endsWith(sep) ? dir + name : dir + sep + name;
}

function source(restoreId) {
  return {
    roots: () => api.restores.entryRoots(restoreId).then((entries) => entries.map((e) => ({ ...e, path: e.name }))),
    list: (path, offset, limit, parent) =>
      api.restores.entryChildren(restoreId, parent.id, offset, limit).then((page) => ({
        ...page,
        entries: page.entries.map((e) => ({ ...e, path: joinPath(path, e.name) })),
      })),
  };
}

function decorateRow(el, entry) {
  el.classList.remove(...CLASSES);
  if (entry.status !== "ok") el.classList.add(`restore-${entry.status}`);
  if (entry.errorsBelow) el.classList.add("below-errors");
  const outcome = el.querySelector(".cell.modified");
  if (outcome) {
    outcome.textContent = entry.kind === "dir" && entry.status === "ok" ? "" : OUTCOME_TEXT[entry.status] ?? entry.status;
    outcome.title = entry.error ?? "";
  }
  el.title = entry.error ?? "";
}

function describe(row) {
  if (!row) return "";
  if (row.type !== "node") return row.node.entry?.path ?? "";
  const e = row.node.entry;
  const parts = [e.path];
  if (e.kind === "file" || e.status !== "ok") parts.push(OUTCOME_TEXT[e.status] ?? e.status);
  if (e.error) parts.push(e.error);
  if (e.errorsBelow) parts.push("errors below");
  return parts.join(" · ");
}

export function createRestoreView() {
  const containerEl = $("restoreview-tree-container");
  const statusEl = $("restoreview-selection");
  let tree = null;
  let treeEl = null;
  let restoreId = null;

  $("restoreview-collapse-all").addEventListener("click", () => {
    tree?.collapseAll();
    treeEl?.focus();
  });

  return {
    open(restore, pageSize) {
      $("restoreview-title").textContent = `Restore #${restore.id}`;
      const p = restore.progress;
      $("restoreview-target").textContent =
        `→ ${restore.target} · ${formatCount(p.filesOk)} restored, ${formatCount(p.filesFailed)} failed`;
      if (restoreId === restore.id) return;
      restoreId = restore.id;
      treeEl = document.createElement("div");
      treeEl.className = "tree";
      treeEl.tabIndex = 0;
      treeEl.setAttribute("role", "tree");
      treeEl.setAttribute("aria-label", `Restore #${restore.id}`);
      containerEl.replaceChildren(treeEl);
      tree = new FileTree(treeEl, source(restore.id), {
        pageSize,
        selectable: false,
        decorateRow,
        onActiveChange: (row) => {
          statusEl.textContent = describe(row);
        },
      });
      tree.start();
    },
    /** Forgets the open restore (its backup was deleted). */
    close() {
      restoreId = null;
      tree = treeEl = null;
      containerEl.replaceChildren();
    },
    get restoreId() {
      return restoreId;
    },
    focus: () => treeEl?.focus(),
  };
}
