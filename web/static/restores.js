// "Restores" view: the running (or latest) restore with progress and outcomes,
// the restore history, and the dialog to start a restore with a folder picker.

import { api } from "./api.js";
import { Estimator, formatRemaining } from "./eta.js";
import { formatCount, formatDuration, formatRate, formatSize } from "./format.js";
import { FileTree } from "./tree.js";

const $ = (id) => document.getElementById(id);
const dateTime = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

const STATUS_LABELS = {
  running: "Restoring",
  pausing: "Pausing…",
  paused: "Paused",
  stopping: "Stopping…",
  completed: "Completed",
  stopped: "Stopped",
  failed: "Failed",
  interrupted: "Interrupted",
};

const ACTIVE_RESTORE = new Set(["running", "pausing", "paused", "stopping"]);

function badge(el, status) {
  el.textContent = STATUS_LABELS[status] ?? status;
  el.className = `badge status-${status === "running" ? "running" : status}`;
}

const duration = (c, now = Date.now()) => (c.finishedAt ? Date.parse(c.finishedAt) : now) - Date.parse(c.startedAt);

/**
 * @param {object} hooks { onBrowse(restore) }
 */
export function createRestoresView({ onBrowse }) {
  let shown = null;
  const estimator = new Estimator();

  const pauseEl = $("restore-pause");
  const stopEl = $("restore-stop");
  const browseEl = $("restore-browse");

  function render(c) {
    $("restore-empty").hidden = c != null;
    $("restore-card").hidden = c == null;
    if (!c) return;
    const p = c.progress;
    const active = ACTIVE_RESTORE.has(c.status);
    $("restore-title").textContent = `Restore #${c.id} of backup #${c.backupId}`;
    badge($("restore-status"), c.status);
    $("restore-target").textContent = `→ ${c.target}`;
    $("restore-ok").textContent = formatCount(p.filesOk);
    $("restore-failed").textContent = formatCount(p.filesFailed);

    // Failed files still count as handled; only restored bytes count, so the estimate follows the real restore speed.
    const running = c.status === "running";
    estimator.sample(c.id, running, p.filesDone, p.bytesDone);
    let line = `${formatSize(p.bytesDone)} of ${formatSize(p.bytesTotal)} · ${formatCount(p.filesDone)} of ${formatCount(p.filesTotal)} files`;
    const rate = running ? estimator.byteRate : null;
    if (rate != null) line += ` · ${formatRate(rate)}`;
    line += ` · ${c.status === "completed" ? "took" : "elapsed"} ${formatDuration(duration(c))}`;
    $("restore-line").textContent = line;
    $("restore-eta").textContent = running
      ? formatRemaining(estimator.remaining(p.filesTotal - p.filesDone, p.bytesTotal - p.bytesDone))
      : "";

    const pct = c.status === "completed" ? 100
      : p.bytesTotal > 0 ? Math.min(100, (p.bytesDone / p.bytesTotal) * 100)
      : p.filesTotal > 0 ? Math.min(100, (p.filesDone / p.filesTotal) * 100)
      : 0;
    $("restore-bar").style.width = `${pct}%`;
    $("restore-bar").parentElement.setAttribute("aria-valuenow", String(Math.round(pct)));

    $("restore-error").hidden = !c.error;
    $("restore-error").textContent = c.error ?? "";
    pauseEl.hidden = stopEl.hidden = !active;
    pauseEl.textContent = c.status === "paused" ? "Resume" : "Pause";
    pauseEl.disabled = c.status !== "running" && c.status !== "paused";
    stopEl.disabled = c.status === "stopping";
    browseEl.hidden = active && c.status !== "paused"; // a paused restore shows what it has done
  }

  function historyRow(c) {
    const tr = document.createElement("tr");
    const cell = (text, cls = "") => {
      const td = document.createElement("td");
      td.textContent = text;
      if (cls) td.className = cls;
      return td;
    };
    const statusCell = document.createElement("td");
    const b = document.createElement("span");
    badge(b, c.status);
    statusCell.append(b);
    if (c.error) statusCell.title = c.error;
    const target = cell(c.target, "paths path");
    target.title = c.target;
    const actions = document.createElement("td");
    actions.className = "actions";
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "primary";
    btn.textContent = ACTIVE_RESTORE.has(c.status) ? "Progress" : "Browse";
    btn.addEventListener("click", () => (ACTIVE_RESTORE.has(c.status) ? show(c) : onBrowse(c)));
    actions.append(btn);
    const p = c.progress;
    tr.append(
      cell(String(c.id)),
      cell(`#${c.backupId}`),
      target,
      cell(dateTime.format(new Date(c.startedAt))),
      cell(formatDuration(duration(c))),
      statusCell,
      cell(formatCount(p.filesOk), "num"),
      cell(formatCount(p.filesFailed), p.filesFailed ? "num bad" : "num"),
      cell(formatSize(p.bytesDone), "num"),
      actions,
    );
    return tr;
  }

  function show(c) {
    shown = c;
    render(c);
  }

  async function act(action) {
    if (!shown) return;
    pauseEl.disabled = stopEl.disabled = true;
    try {
      show(await api.restores[action](shown.id));
    } catch (err) {
      $("restore-error").hidden = false;
      $("restore-error").textContent = `Could not ${action}: ${err.message}`;
    }
  }

  pauseEl.addEventListener("click", () => act(shown?.status === "paused" ? "resume" : "pause"));
  stopEl.addEventListener("click", () => act("stop"));
  browseEl.addEventListener("click", () => shown && onBrowse(shown));

  return {
    show,
    get shownId() {
      return shown?.id ?? null;
    },
    renderHistory(restores) {
      $("restore-history-empty").hidden = restores.length > 0;
      $("restore-history-table").hidden = restores.length === 0;
      $("restore-history-body").replaceChildren(...restores.map(historyRow));
    },
  };
}

/** Lets the user pick a folder on any drive; resolves the path or null. */
function pickFolder() {
  const dialogEl = $("picker-dialog");
  const pathEl = $("picker-path");
  const containerEl = $("picker-tree-container");
  const treeEl = document.createElement("div");
  treeEl.className = "tree";
  treeEl.tabIndex = 0;
  treeEl.setAttribute("role", "tree");
  treeEl.setAttribute("aria-label", "Folders");
  containerEl.replaceChildren(treeEl);

  let current = null;
  const folderOf = (row) => {
    if (row?.type !== "node") return null;
    const { entry } = row.node;
    if (entry.kind === "dir") return entry.path;
    return row.node.parent?.entry?.path ?? null; // a file: its folder
  };
  const tree = new FileTree(treeEl, api, {
    pageSize: 200,
    selectable: false,
    onActiveChange: (row) => {
      current = folderOf(row);
      pathEl.textContent = current ?? "Select a folder";
      $("picker-use").disabled = current == null;
    },
  });
  tree.start();

  return new Promise((resolve) => {
    const finish = (value) => {
      dialogEl.hidden = true;
      dialogEl.removeEventListener("keydown", onKey);
      resolve(value);
    };
    const onKey = (e) => {
      if (e.key === "Escape") {
        e.preventDefault();
        finish(null);
      }
    };
    $("picker-use").onclick = () => finish(current);
    $("picker-cancel").onclick = () => finish(null);
    dialogEl.addEventListener("keydown", onKey);
    dialogEl.hidden = false;
    treeEl.focus();
  });
}

/**
 * Asks for the target and starts a restore of backupId. Resolves the started
 * restore, or null when canceled.
 */
export function openRestoreDialog(backupId) {
  const dialogEl = $("restore-dialog");
  const inputEl = $("restore-target-input");
  const errorEl = $("restore-dialog-error");
  const startEl = $("restore-dialog-start");
  const previousFocus = document.activeElement;
  $("restore-dialog-title").textContent = `Restore backup #${backupId}`;
  errorEl.hidden = true;

  return new Promise((resolve) => {
    const finish = (value) => {
      dialogEl.hidden = true;
      dialogEl.removeEventListener("keydown", onKey);
      if (previousFocus instanceof HTMLElement) previousFocus.focus();
      resolve(value);
    };
    const start = async () => {
      const target = inputEl.value.trim();
      if (!target) {
        errorEl.hidden = false;
        errorEl.textContent = "Choose a target folder.";
        return;
      }
      startEl.disabled = true;
      try {
        finish(await api.restores.start(backupId, target));
      } catch (err) {
        errorEl.hidden = false;
        errorEl.textContent = err.message;
      } finally {
        startEl.disabled = false;
      }
    };
    const onKey = (e) => {
      if (e.key === "Escape" && $("picker-dialog").hidden) {
        e.preventDefault();
        finish(null);
      } else if (e.key === "Enter" && e.target === inputEl) {
        e.preventDefault();
        start();
      }
    };
    $("restore-pick").onclick = async () => {
      const picked = await pickFolder();
      if (picked) inputEl.value = picked;
      inputEl.focus();
    };
    $("restore-dialog-cancel").onclick = () => finish(null);
    startEl.onclick = start;
    dialogEl.addEventListener("keydown", onKey);
    dialogEl.hidden = false;
    inputEl.focus();
    inputEl.select();
  });
}
