// "Backups" view: all backups, newest first, and deleting them.

import { api } from "./api.js";
import { confirmDialog } from "./confirm.js";
import { formatCount, formatDuration, formatSize } from "./format.js";
import { ACTIVE_STATUSES, backupDuration, statusBadge } from "./progress.js";

const dateTime = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

function cell(text, className = "") {
  const td = document.createElement("td");
  td.textContent = text;
  if (className) td.className = className;
  return td;
}

/**
 * @param {object} hooks { onShowPaths(backup), onShowProgress(backup), onBrowse(backup), onDeleted(backup) }
 */
export function createHistoryView({ onShowPaths, onShowProgress, onBrowse, onDeleted }) {
  const tableEl = document.getElementById("history-table");
  const bodyEl = document.getElementById("history-body");
  const emptyEl = document.getElementById("history-empty");
  const noticeEl = document.getElementById("history-notice");
  let deleting = null; // id of the backup being deleted
  let lastBackups = [];

  function row(backup) {
    const tr = document.createElement("tr");
    const p = backup.progress;

    const status = document.createElement("span");
    statusBadge(status, backup.status);
    const statusCell = document.createElement("td");
    statusCell.append(status);
    if (backup.error) statusCell.title = backup.error;

    const paths = document.createElement("td");
    paths.className = "paths";
    const first = document.createElement("span");
    first.className = "path";
    first.textContent = backup.roots[0] ?? "";
    paths.append(first);
    if (backup.roots.length > 1) {
      const more = document.createElement("span");
      more.className = "more-paths";
      more.textContent = ` +${backup.roots.length - 1} more`;
      paths.append(more);
    }
    paths.title = backup.roots.join("\n");

    const actions = document.createElement("td");
    actions.className = "actions";
    const showPaths = document.createElement("button");
    showPaths.type = "button";
    showPaths.textContent = "Show paths";
    showPaths.title = "Select this backup's paths in New backup";
    showPaths.addEventListener("click", () => onShowPaths(backup));
    actions.append(showPaths);
    if (backup.status === "completed" && backup.ready) {
      const browse = document.createElement("button");
      browse.type = "button";
      browse.className = "primary";
      browse.textContent = "Browse";
      browse.title = "Browse this backup, exclude items and restore the rest";
      browse.addEventListener("click", () => onBrowse(backup));
      actions.prepend(browse);
    }
    if (ACTIVE_STATUSES.has(backup.status)) {
      const progress = document.createElement("button");
      progress.type = "button";
      progress.textContent = "Progress";
      progress.addEventListener("click", () => onShowProgress(backup));
      actions.prepend(progress);
    } else {
      const del = document.createElement("button");
      del.type = "button";
      del.className = "danger";
      del.textContent = deleting === backup.id ? "Deleting…" : "Delete…";
      del.title = "Delete this backup with its exclusions and restore records";
      del.disabled = deleting != null;
      del.addEventListener("click", () => deleteBackup(backup));
      actions.append(del);
    }

    tr.append(
      cell(String(backup.id)),
      cell(dateTime.format(new Date(backup.startedAt))),
      cell(formatDuration(backupDuration(backup))),
      statusCell,
      cell(`${formatCount(p.dirsDone)} / ${formatCount(p.dirsFound)}`, "num"),
      cell(`${formatCount(p.filesDone)} / ${formatCount(p.filesFound)}`, "num"),
      cell(formatSize(p.bytesDone), "num"),
      cell(formatCount(p.errors), "num"),
      paths,
      actions,
    );
    return tr;
  }

  function notice(text, isError = false) {
    noticeEl.hidden = !text;
    noticeEl.textContent = text;
    noticeEl.classList.toggle("error", isError);
  }

  async function deleteBackup(backup) {
    const p = backup.progress;
    const items = p.dirsDone + p.filesDone + p.errors;
    const restores = (await api.restores.list().catch(() => [])).filter((r) => r.backupId === backup.id).length;
    const what = restores
      ? `Its ${formatCount(items)} items, their exclusions and the records of its ${restores === 1 ? "restore" : `${restores} restores`}`
      : `Its ${formatCount(items)} items and their exclusions`;
    const ok = await confirmDialog({
      title: `Delete backup #${backup.id}?`,
      text: `${what} are removed from the database, with the stored file contents no other backup uses. Files on disk, including restored files, are not touched. This cannot be undone.`,
      okLabel: "Delete backup",
    });
    if (!ok) return;
    deleting = backup.id;
    render(lastBackups);
    notice(`Deleting backup #${backup.id}… (a whole drive can take a minute)`);
    try {
      const d = await api.backups.delete(backup.id);
      notice(
        `Deleted backup #${backup.id}: ${formatCount(d.items)} items` +
          (d.restores ? `, ${formatCount(d.restores)} restore ${d.restores === 1 ? "record" : "records"}` : "") +
          (d.bytesFreed ? `, ${formatSize(d.bytesFreed)} of stored contents freed` : "") +
          ".",
      );
      onDeleted(backup);
    } catch (err) {
      notice(`Could not delete backup #${backup.id}: ${err.message}`, true);
    } finally {
      deleting = null;
      render(await api.backups.list().catch(() => lastBackups));
    }
  }

  function render(backups) {
    lastBackups = backups;
    emptyEl.hidden = backups.length > 0;
    tableEl.hidden = backups.length === 0;
    bodyEl.replaceChildren(...backups.map(row));
  }

  return { render };
}
