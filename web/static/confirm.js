// Small modal confirmation for irreversible actions. Cancel has focus
// first, so a stray Enter never confirms; Esc cancels.

const $ = (id) => document.getElementById(id);

/** Resolves true when confirmed, false otherwise. */
export function confirmDialog({ title, text, okLabel }) {
  const dialogEl = $("confirm-dialog");
  const okEl = $("confirm-ok");
  const cancelEl = $("confirm-cancel");
  $("confirm-title").textContent = title;
  $("confirm-text").textContent = text;
  okEl.textContent = okLabel;
  const previousFocus = document.activeElement;

  return new Promise((resolve) => {
    const finish = (ok) => {
      dialogEl.hidden = true;
      dialogEl.removeEventListener("keydown", onKey);
      okEl.onclick = cancelEl.onclick = null;
      if (previousFocus instanceof HTMLElement) previousFocus.focus();
      resolve(ok);
    };
    const onKey = (e) => {
      if (e.key === "Escape") {
        e.preventDefault();
        finish(false);
      } else if (e.key === "Tab") {
        // Keep focus on the two buttons.
        e.preventDefault();
        (document.activeElement === okEl ? cancelEl : okEl).focus();
      }
    };
    okEl.onclick = () => finish(true);
    cancelEl.onclick = () => finish(false);
    dialogEl.addEventListener("keydown", onKey);
    dialogEl.hidden = false;
    cancelEl.focus();
  });
}
