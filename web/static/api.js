// Thin client for the Go backend's JSON API. It also tracks whether the
// backend is reachable, so the app can show a "backend not available" page.

const TIMEOUT_MS = 15_000;
const STATE_TIMEOUT_MS = 2 * 60_000; // state of many loaded items
const DELETE_TIMEOUT_MS = 10 * 60_000; // a backup of a whole drive takes seconds to delete

const statusListeners = new Set();
let backendUp = true;

/** Calls fn(up) whenever the backend becomes reachable or unreachable. */
export function onBackendStatus(fn) {
  statusListeners.add(fn);
}

function setBackendUp(up) {
  if (up === backendUp) return;
  backendUp = up;
  for (const fn of statusListeners) fn(up);
}

async function request(method, path, { params = {}, body, timeout = TIMEOUT_MS } = {}) {
  const url = new URL(path, location.origin);
  for (const [key, value] of Object.entries(params)) {
    url.searchParams.set(key, String(value));
  }
  const init = { method, headers: { Accept: "application/json" } };
  if (body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }

  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeout);
  let res;
  try {
    res = await fetch(url, { ...init, signal: controller.signal });
  } catch (err) {
    setBackendUp(false);
    throw new Error(err.name === "AbortError" ? "The backend did not respond in time" : "The backend is not reachable");
  } finally {
    clearTimeout(timer);
  }
  setBackendUp(true); // any HTTP response means the server is there

  let data = null;
  try {
    data = await res.json();
  } catch {
    // Non-JSON body; fall back to the status text below.
  }
  if (!res.ok) {
    const err = new Error(data?.message || `${res.status} ${res.statusText}`);
    err.status = res.status;
    throw err;
  }
  return data;
}

export const api = {
  /** @returns {Promise<Entry[]>} drive letters on Windows, "/" elsewhere */
  roots: () => request("GET", "/api/roots").then((b) => b.entries),

  /** One page of a directory; the backend caps limit at 1000. */
  list: (path, offset, limit) => request("GET", "/api/list", { params: { path, offset, limit } }),

  restores: {
    /** Starts restoring a backup into target (a new or empty folder). */
    start: (backupId, target) => request("POST", `/api/backups/${backupId}/restore`, { body: { target } }),
    list: () => request("GET", "/api/restores").then((b) => b.restores),
    /** The running or paused restore, or null. */
    current: () => request("GET", "/api/restores/current").then((b) => b.restore),
    get: (id) => request("GET", `/api/restores/${id}`),
    pause: (id) => request("POST", `/api/restores/${id}/pause`),
    resume: (id) => request("POST", `/api/restores/${id}/resume`),
    stop: (id) => request("POST", `/api/restores/${id}/stop`),
    entryRoots: (id) => request("GET", `/api/restores/${id}/entries/roots`).then((b) => b.entries),
    entryChildren: (id, parent, offset, limit) =>
      request("GET", `/api/restores/${id}/entries/children`, { params: { parent, offset, limit } }),
  },

  backups: {
    list: () => request("GET", "/api/backups").then((b) => b.backups),
    /** The running or paused backup, or null. */
    current: () => request("GET", "/api/backups/current").then((b) => b.backup),
    get: (id) => request("GET", `/api/backups/${id}`),
    start: (roots) => request("POST", "/api/backups", { body: { roots } }),
    pause: (id) => request("POST", `/api/backups/${id}/pause`),
    resume: (id) => request("POST", `/api/backups/${id}/resume`),
    stop: (id) => request("POST", `/api/backups/${id}/stop`),
    /** Deletes a finished backup with everything derived from it; returns {items, restores, names, contents, bytesFreed}. */
    delete: (id) => request("DELETE", `/api/backups/${id}`, { timeout: DELETE_TIMEOUT_MS }).then((b) => b.deleted),
    /** Root items of a completed backup. */
    entryRoots: (id) => request("GET", `/api/backups/${id}/entries/roots`).then((b) => b.entries),
    /** One page of a backed-up directory's children, by item id. */
    entryChildren: (id, parent, offset, limit) =>
      request("GET", `/api/backups/${id}/entries/children`, { params: { parent, offset, limit } }),
    /** Removes all exclusions of a backup; returns {removed}. */
    reset: (id) => request("POST", `/api/backups/${id}/entries/reset`, { body: {} }),
    /** Sets (on) or clears the exclusion mark of items. */
    exclude: (id, items, on) => request("POST", `/api/backups/${id}/entries/exclude`, { body: { items, on } }),
    /** Current entries for the given item ids. */
    state: (id, items) =>
      request("POST", `/api/backups/${id}/entries/state`, { body: { items }, timeout: STATE_TIMEOUT_MS }).then((b) => b.entries),
  },
};
