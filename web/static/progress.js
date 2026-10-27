// "Progress" view: one bar with a time estimate for the active backup, or
// the latest one.

import { api } from "./api.js";
import { Estimator, formatRemaining } from "./eta.js";
import { formatCount, formatDuration, formatRate, formatSize } from "./format.js";

const $ = (id) => document.getElementById(id);

const STATUS_LABELS = {
  running: "Running",
  pausing: "Pausing…",
  paused: "Paused",
  stopping: "Stopping…",
  analyzing: "Finding duplicates…",
  completed: "Completed",
  stopped: "Stopped",
  failed: "Failed",
  interrupted: "Interrupted",
};

export const ACTIVE_STATUSES = new Set(["running", "pausing", "paused", "stopping", "analyzing"]);

export function statusBadge(el, status) {
  el.textContent = STATUS_LABELS[status] ?? status;
  el.className = `badge status-${status}`;
}

export function backupDuration(backup, now = Date.now()) {
  const end = backup.finishedAt ? Date.parse(backup.finishedAt) : now;
  return end - Date.parse(backup.startedAt);
}

/**
 * @param {object} hooks { onBrowse(backup) }
 */
export function createProgressView({ onBrowse }) {
  let shown = null; // backup currently displayed
  const estimator = new Estimator();

  const pauseEl = $("pause-backup");
  const stopEl = $("stop-backup");
  const browseEl = $("progress-browse");

  function render(backup) {
    $("progress-empty").hidden = backup != null;
    $("progress-card").hidden = backup == null;
    if (!backup) return;

    const p = backup.progress;
    const active = ACTIVE_STATUSES.has(backup.status);
    $("progress-title").textContent = `Backup #${backup.id}`;
    statusBadge($("progress-status"), backup.status);

    // Phases: finding files (totals still growing, so no percentage),
    // hashing (size hashed of the known total), finding duplicates.
    const discovering = active && p.discovering;
    const analyzing = backup.status === "analyzing";
    const running = backup.status === "running";
    const handled = p.filesDone + p.errors;
    estimator.sample(`${backup.id} ${discovering}`, running && !discovering, handled, p.bytesDone);

    let line, eta = "";
    if (discovering) {
      line = `Finding files… ${formatCount(p.filesFound)} files, ${formatSize(p.bytesFound)} so far`;
    } else if (analyzing) {
      line = `All ${formatCount(p.filesFound)} files hashed. Finding duplicates…`;
    } else {
      line = `${formatSize(p.bytesDone)} of ${formatSize(p.bytesFound)} · ${formatCount(handled)} of ${formatCount(p.filesFound)} files`;
      const rate = running ? estimator.byteRate : null;
      if (rate != null) line += ` · ${formatRate(rate)}`;
      if (running) eta = formatRemaining(estimator.remaining(p.filesFound - handled, p.bytesFound - p.bytesDone));
    }
    $("progress-line").textContent = line;
    $("progress-eta").textContent = eta;

    const detail = [`${backup.status === "completed" ? "Took" : "Elapsed"} ${formatDuration(backupDuration(backup))}`];
    if (p.errors > 0) detail.push(`${formatCount(p.errors)} unreadable (left out)`);
    $("progress-detail").textContent = detail.join(" · ");

    // Unreadable files never count as hashed bytes, so a completed backup may
    // end slightly below its found size.
    const pct = discovering || analyzing || backup.status === "completed" ? 100
      : p.bytesFound > 0 ? Math.min(100, (p.bytesDone / p.bytesFound) * 100)
      : p.filesFound > 0 ? Math.min(100, (handled / p.filesFound) * 100)
      : 0;
    $("progress-bar").style.width = `${pct}%`;
    $("progress-bar").parentElement.setAttribute("aria-valuenow", String(Math.round(pct)));
    $("progress-bar").classList.toggle("indeterminate", discovering || analyzing);

    const errorEl = $("progress-error");
    errorEl.hidden = !backup.error;
    errorEl.textContent = backup.error ?? "";

    pauseEl.hidden = !active;
    stopEl.hidden = !active;
    pauseEl.textContent = backup.status === "paused" ? "Resume" : "Pause";
    pauseEl.disabled = backup.status !== "running" && backup.status !== "paused";
    stopEl.disabled = backup.status === "stopping" || backup.status === "analyzing";
    browseEl.hidden = !(backup.status === "completed" && backup.ready);

    const rootsEl = $("progress-roots");
    if (rootsEl.dataset.backupId !== String(backup.id)) {
      rootsEl.dataset.backupId = String(backup.id);
      rootsEl.replaceChildren(
        ...backup.roots.map((root) => {
          const li = document.createElement("li");
          li.textContent = root;
          return li;
        }),
      );
    }
  }

  async function act(action) {
    if (!shown) return;
    pauseEl.disabled = stopEl.disabled = true;
    try {
      render(await api.backups[action](shown.id));
    } catch (err) {
      $("progress-error").hidden = false;
      $("progress-error").textContent = `Could not ${action}: ${err.message}`;
    }
  }

  browseEl.addEventListener("click", () => shown && onBrowse(shown));
  pauseEl.addEventListener("click", () => act(shown?.status === "paused" ? "resume" : "pause"));
  stopEl.addEventListener("click", () => act("stop"));

  return {
    /** Shows a backup (active or finished); null shows the empty state. */
    show(backup) {
      shown = backup;
      render(backup);
    },
    get shownId() {
      return shown?.id ?? null;
    },
  };
}
