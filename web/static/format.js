// Display formatting shared by all views: sizes, counts, dates, durations.

const units = ["B", "KB", "MB", "GB", "TB", "PB"];
const decimal = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 });
const integer = new Intl.NumberFormat();
const dateTime = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

export function formatSize(bytes) {
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return unit === 0 ? `${bytes} B` : `${decimal.format(value)} ${units[unit]}`;
}

export function formatBytesExact(bytes) {
  return `${integer.format(bytes)} bytes`;
}

export function formatDate(iso) {
  return iso ? dateTime.format(new Date(iso)) : "";
}

export function formatCount(n) {
  return integer.format(n);
}

/** "1:02:03" or "2:03" for a duration in milliseconds. */
export function formatDuration(ms) {
  const total = Math.max(0, Math.round(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = String(total % 60).padStart(2, "0");
  return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${s}` : `${m}:${s}`;
}

export function formatRate(bytesPerSecond) {
  return `${formatSize(Math.round(bytesPerSecond))}/s`;
}
