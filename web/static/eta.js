// Remaining-time estimate for a job that works through known totals of
// files and bytes. Rates are measured over the last minute (long enough
// to smooth bursts, short enough to follow a change of speed). Time is
// estimated from both the byte rate and the file rate and the larger
// estimate wins: many small files are slow per byte, big files per file.

const WINDOW_MS = 60_000;
const MIN_SPAN_MS = 3_000; // too little history gives wild estimates

export class Estimator {
  #key = null;
  #samples = []; // { t, files, bytes }

  /**
   * Records progress. key identifies a phase of one job (e.g. "backup 4
   * hashing"); a new key, or active = false (paused), starts over.
   */
  sample(key, active, files, bytes) {
    const t = performance.now();
    if (key !== this.#key || !active) {
      this.#key = active ? key : null;
      this.#samples = active ? [{ t, files, bytes }] : [];
      return;
    }
    this.#samples.push({ t, files, bytes });
    while (this.#samples.length > 2 && t - this.#samples[1].t > WINDOW_MS) this.#samples.shift();
  }

  /** Bytes per second over the window, or null while unknown. */
  get byteRate() {
    const span = this.#span();
    return span ? span.bytes / span.s : null;
  }

  /** Estimated milliseconds left, or null while unknown. */
  remaining(filesLeft, bytesLeft) {
    const span = this.#span();
    if (!span) return null;
    if (filesLeft <= 0 && bytesLeft <= 0) return 0;
    const byFiles = span.files > 0 ? filesLeft / (span.files / span.s) : Infinity;
    const byBytes = span.bytes > 0 ? bytesLeft / (span.bytes / span.s) : bytesLeft > 0 ? Infinity : 0;
    const s = Math.max(byFiles, byBytes);
    return Number.isFinite(s) ? s * 1000 : null;
  }

  #span() {
    const first = this.#samples[0];
    const last = this.#samples.at(-1);
    if (!first || last.t - first.t < MIN_SPAN_MS) return null;
    return { s: (last.t - first.t) / 1000, files: last.files - first.files, bytes: last.bytes - first.bytes };
  }
}

/** "about 2 h 5 min left": rounded so the number does not jitter. */
export function formatRemaining(ms) {
  if (ms == null) return "estimating time left…";
  const s = ms / 1000;
  if (s < 10) return "a few seconds left";
  if (s < 60) return `about ${Math.ceil(s / 10) * 10} s left`;
  const min = s / 60;
  if (min < 10) return `about ${Math.ceil(min)} min left`;
  if (min < 60) return `about ${Math.ceil(min / 5) * 5} min left`;
  const h = Math.floor(min / 60);
  const m = Math.ceil((min % 60) / 10) * 10;
  return m === 60 ? `about ${h + 1} h left` : m === 0 ? `about ${h} h left` : `about ${h} h ${m} min left`;
}
