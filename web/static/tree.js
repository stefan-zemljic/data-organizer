// Lazily loaded file tree following the WAI-ARIA tree pattern: the tree
// element keeps focus and points at the active row via aria-activedescendant.
// Multi-selection is tracked by path, so it survives collapsing and reloading;
// items inside a selected folder count as selected without being listed.

import { formatBytesExact, formatCount, formatDate, formatSize } from "./format.js";

const ICONS = {
  twisty: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M6 3.5 10.5 8 6 12.5"/></svg>',
  check: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M4 8.5 7 11.5 12 5"/></svg>',
  dir: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M1.5 3.25c0-.41.34-.75.75-.75h3.6l1.5 1.5h6.4c.41 0 .75.34.75.75v8c0 .41-.34.75-.75.75H2.25a.75.75 0 0 1-.75-.75z"/></svg>',
  file: '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M3.25 2c0-.41.34-.75.75-.75h5.5l3.75 3.75v9c0 .41-.34.75-.75.75H4a.75.75 0 0 1-.75-.75z"/></svg>',
};

const TYPEAHEAD_RESET_MS = 700;

// Rows rendered above and below the visible part of the tree.
const OVERSCAN = 20;

let nextId = 0;

function makeNode(entry, parent) {
  return {
    id: ++nextId,
    entry,
    parent,
    depth: parent ? parent.depth + 1 : -1,
    children: [],
    expanded: false,
    loaded: false,
    loading: null, // in-flight load promise
    error: null,
    total: 0,
    nextOffset: 0,
  };
}

// Unreadable directories (backup entries with an error) have no children.
const isDir = (node) => node.entry?.kind === "dir" && !node.entry.error;

/** Is path equal to, or inside, the directory at dirPath? */
function isWithin(path, dirPath) {
  if (path === dirPath) return true;
  const sep = dirPath.includes("\\") ? "\\" : "/";
  return path.startsWith(dirPath.endsWith(sep) ? dirPath : dirPath + sep);
}

export class FileTree {
  /**
   * @param {HTMLElement} el      element with role="tree"
   * @param {object} api          { roots(), list(path, offset, limit, entry) }
   * @param {object} options      { pageSize, onActiveChange(row), onSelectionChange(selection),
   *                                decorateRow(el, entry) for extra classes on item rows,
   *                                selectable (false: no selection at all),
   *                                onKey(event, row) returning true when it handled the key,
   *                                onRowsRendered(entries) after the visible window changed }
   */
  constructor(el, api, {
    pageSize = 200,
    onActiveChange = () => {},
    onSelectionChange = () => {},
    decorateRow = () => {},
    selectable = true,
    onKey = () => false,
    onRowsRendered = () => {},
  } = {}) {
    this.el = el;
    this.api = api;
    this.pageSize = pageSize;
    this.onActiveChange = onActiveChange;
    this.onSelectionChange = onSelectionChange;
    this.decorateRow = decorateRow;
    this.selectable = selectable;
    this.onKey = onKey;
    this.onRowsRendered = onRowsRendered;

    // Virtual scrolling: a spacer as tall as all rows, holding a window of
    // the rendered ones (see renderWindow).
    this.rowHeight = parseFloat(getComputedStyle(el).getPropertyValue("--row-h")) || 26;
    this.spacer = document.createElement("div");
    this.spacer.className = "tree-spacer";
    this.windowEl = document.createElement("div");
    this.windowEl.className = "tree-window";
    this.spacer.append(this.windowEl);
    el.replaceChildren(this.spacer);
    this.windowStart = 0;
    this.windowEnd = 0;
    this.dirty = true;
    let frame = 0;
    el.addEventListener("scroll", () => {
      if (frame) return;
      frame = requestAnimationFrame(() => {
        frame = 0;
        this.renderWindow();
        if (this.drag) this.updateDrag(); // the row under the mouse changed
      });
    });

    this.root = makeNode(null, null);
    this.root.expanded = true;
    this.rows = [];
    this.rowIndex = new Map();
    this.rowEls = new Map();
    this.activeKey = null;
    /** @type {Map<string, object>} selected entries by path */
    this.selection = new Map();
    /**
     * Mouse drag in progress (selectable trees): rows fromKey..toKey are shown
     * as selected (select) or deselected until mouse up commits them; replace
     * also drops the rest of the selection on commit.
     */
    this.drag = null;
    /** Esc hid the cursor highlight; the next key or click shows it again. */
    this.cursorHidden = false;
    /** Start of the focused range in non-selectable trees (Shift+move/click). */
    this.rangeAnchorKey = null;
    this.typeahead = "";
    this.typeaheadTimer = 0;

    el.addEventListener("keydown", (e) => this.onKeyDown(e));
    el.addEventListener("click", (e) => this.onClick(e));
    if (selectable) el.addEventListener("mousedown", (e) => this.onMouseDown(e));
    el.addEventListener("dblclick", (e) => this.onDoubleClick(e));
  }

  start() {
    return this.load(this.root);
  }

  // ---- Data loading -------------------------------------------------------

  /** Loads (or with append, extends) node's children. Concurrent calls share one request. */
  load(node, { append = false } = {}) {
    if (node.loading) return node.loading;

    const loadingKey = `l${node.id}`;
    const firstNew = append ? node.children.length : 0;
    if (this.activeKey === `m${node.id}` || this.activeKey === `e${node.id}`) this.activeKey = loadingKey;

    node.error = null;
    node.loading = this.fetchChildren(node, append).then(() => {
      node.loading = null;
      if (this.activeKey === loadingKey) {
        // Keep the cursor where the user was: on the first new entry after
        // "load more", or on the error row if loading failed.
        const target = node.error ? null : node.children[firstNew];
        this.activeKey = node.error ? `e${node.id}` : target ? `n${target.id}` : this.keyOf(node);
      }
      this.render();
    });
    this.render();
    return node.loading;
  }

  async fetchChildren(node, append) {
    try {
      if (node === this.root) {
        const entries = await this.api.roots();
        node.children = entries.map((e) => makeNode(e, node));
        node.total = node.nextOffset = node.children.length;
      } else {
        const offset = append ? node.nextOffset : 0;
        const page = await this.api.list(node.entry.path, offset, this.pageSize, node.entry);
        const added = page.entries.map((e) => makeNode(e, node));
        node.children = append ? node.children.concat(added) : added;
        node.total = page.total;
        node.nextOffset = page.nextOffset;
      }
      node.loaded = true;
    } catch (err) {
      node.error = err.message || String(err);
    }
  }

  loadMore(node) {
    return this.load(node, { append: true });
  }

  refresh(node) {
    if (node.loading) return node.loading;
    node.loaded = false;
    if (this.isActiveInside(node)) this.activeKey = this.keyOf(node);
    return this.load(node);
  }

  // ---- Expand / collapse --------------------------------------------------

  expand(node) {
    if (!isDir(node)) return Promise.resolve();
    node.expanded = true;
    if (!node.loaded) return this.load(node);
    this.render();
    return node.loading ?? Promise.resolve();
  }

  collapse(node) {
    if (!node.expanded) return;
    if (this.isActiveInside(node)) this.activeKey = this.keyOf(node);
    node.expanded = false;
    this.render();
  }

  toggle(node) {
    node.expanded ? this.collapse(node) : this.expand(node);
  }

  collapseAll() {
    const walk = (node) => {
      for (const child of node.children) {
        child.expanded = false;
        walk(child);
      }
    };
    let top = this.activeRow()?.node;
    while (top && top.parent && top.parent !== this.root) top = top.parent;
    walk(this.root);
    if (top && top !== this.root) this.activeKey = this.keyOf(top);
    this.render();
  }

  expandSiblings(node) {
    for (const sibling of node.parent?.children ?? []) {
      if (isDir(sibling) && !sibling.expanded) this.expand(sibling);
    }
  }

  // ---- Selection ----------------------------------------------------------

  /** Is the item itself in the selection (not just inside a selected folder)? */
  isSelected(node) {
    return node.entry != null && this.selection.has(node.entry.path);
  }

  /**
   * Is the item shown as selected: it or a loaded ancestor is selected, with
   * a pending drag applied. Derived from the ancestors; nothing below is read.
   */
  looksSelected(node) {
    if (this.selection.size === 0 && !this.drag) return false;
    const [lo, hi] = this.dragBounds();
    for (let n = node; n?.entry; n = n.parent) {
      const i = this.rowIndex.get(this.keyOf(n)) ?? -1;
      if (i >= lo && i <= hi ? this.drag.select : this.isSelected(n)) return true;
    }
    return false;
  }

  setSelected(entry, selected) {
    if (selected) this.selection.set(entry.path, entry);
    else this.selection.delete(entry.path);
  }

  /** Re-renders the window so selection marks (also implied ones) follow. */
  refreshMarks() {
    this.dirty = true;
    this.renderWindow();
  }

  toggleSelected(row) {
    this.setSelected(row.node.entry, !this.isSelected(row.node));
    this.refreshMarks();
    this.onSelectionChange(this.selection);
  }

  /** Adds every visible (loaded, expanded) item; never deselects or loads more. */
  selectAllVisible() {
    for (const row of this.rows) {
      if (row.type === "node") this.setSelected(row.node.entry, true);
    }
    this.refreshMarks();
    this.onSelectionChange(this.selection);
  }

  /** Replaces the selection with the given paths (e.g. a past backup's roots). */
  setSelection(paths) {
    this.selection.clear();
    for (const path of paths) this.selection.set(path, { path });
    this.render();
    this.onSelectionChange(this.selection);
  }

  clearSelection() {
    this.selection.clear();
    this.render();
    this.onSelectionChange(this.selection);
  }

  /**
   * Expands every folder on the way to each selected item, loading further
   * pages where a selected item lies beyond what is loaded, then moves the
   * cursor to the first selected row. Returns the paths that could not be
   * reached (deleted, renamed or no longer accessible).
   */
  async revealSelected() {
    const missing = [];
    for (const path of this.selection.keys()) {
      if (!(await this.reveal(path))) missing.push(path);
    }
    this.render();
    const first = this.rows.find((r) => r.type === "node" && this.isSelected(r.node));
    if (first) {
      this.activeKey = first.key;
      this.applyActive();
    }
    return missing;
  }

  async reveal(path) {
    let node = this.root;
    if (!node.loaded) await this.load(node);
    for (;;) {
      let child = node.children.find((c) => isWithin(path, c.entry.path));
      while (!child && node.loaded && !node.error && node.nextOffset < node.total) {
        await this.loadMore(node);
        child = node.children.find((c) => isWithin(path, c.entry.path));
      }
      if (!child) return false;
      if (child.entry.path === path) return true;
      await this.expand(child);
      if (child.error) return false;
      node = child;
    }
  }

  /** Every loaded entry, expanded or not. */
  *loadedEntries() {
    const stack = [...this.root.children];
    while (stack.length > 0) {
      const node = stack.pop();
      yield node.entry;
      stack.push(...node.children);
    }
  }

  /** Re-applies decorateRow to the rendered rows, e.g. after entries changed. */
  redecorate() {
    for (let i = this.windowStart; i < this.windowEnd; i++) {
      const row = this.rows[i];
      if (row?.type !== "node") continue;
      const el = this.rowEls.get(row.key);
      if (el) this.decorateRow(el, row.node.entry);
    }
    this.onActiveChange(this.activeRow() ?? null);
  }

  // ---- Rows ---------------------------------------------------------------

  keyOf(node) {
    return `n${node.id}`;
  }

  /** Is the active row inside node's subtree (excluding node itself)? */
  isActiveInside(node) {
    const row = this.activeRow();
    if (!row) return false;
    let n = row.type === "node" ? row.node.parent : row.node;
    while (n) {
      if (n === node) return true;
      n = n.parent;
    }
    return false;
  }

  collectRows() {
    const rows = [];
    const visit = (node) => {
      node.children.forEach((child, index) => {
        rows.push({ type: "node", key: `n${child.id}`, node: child, depth: child.depth, index });
        if (child.expanded) visit(child);
      });
      const depth = node.depth + 1;
      if (node.loading) rows.push({ type: "loading", key: `l${node.id}`, node, depth });
      else if (node.error) rows.push({ type: "error", key: `e${node.id}`, node, depth });
      else if (node.loaded && node.nextOffset < node.total) rows.push({ type: "more", key: `m${node.id}`, node, depth });
    };
    visit(this.root);
    return rows;
  }

  /** Recomputes the visible row list after the tree changed. */
  render() {
    const previousIndex = this.activeIndex();
    this.rows = this.collectRows();
    this.rowIndex = new Map(this.rows.map((r, i) => [r.key, i]));
    this.spacer.style.height = `${this.rows.length * this.rowHeight}px`;
    if (!this.rowIndex.has(this.activeKey)) {
      const i = Math.min(Math.max(previousIndex, 0), this.rows.length - 1);
      this.activeKey = this.rows[i]?.key ?? null;
    }
    if (this.rangeAnchorKey != null && !this.rowIndex.has(this.rangeAnchorKey)) this.rangeAnchorKey = null; // collapsed away
    this.dirty = true; // rows changed: rebuild the window even if it did not move
    this.applyActive();
  }

  /**
   * Virtual scrolling: only the rows in view (plus OVERSCAN above and
   * below) exist in the DOM, inside an absolutely positioned window over a
   * spacer as tall as all rows.
   */
  renderWindow() {
    const h = this.rowHeight;
    const top = this.el.scrollTop;
    const visStart = Math.floor(top / h);
    const visEnd = Math.min(this.rows.length, Math.ceil((top + this.el.clientHeight) / h));
    // Keep the current window while the visible rows are inside it; this
    // rebuilds once per OVERSCAN rows of scrolling instead of every row.
    if (!this.dirty && visStart >= this.windowStart && visEnd <= this.windowEnd) return;
    const start = Math.max(0, visStart - OVERSCAN);
    const end = Math.min(this.rows.length, visEnd + OVERSCAN);
    this.dirty = false;
    this.windowStart = start;
    this.windowEnd = end;

    const [lo, hi] = this.rangeBounds();
    const els = new Map();
    const fragment = [];
    const rendered = [];
    for (let i = start; i < end; i++) {
      const row = this.rows[i];
      const el = this.rowEls.get(row.key) ?? this.createRowEl(row);
      this.updateRowEl(el, row);
      el.classList.toggle("active", row.key === this.activeKey);
      if (row.key === this.activeKey) this.activeEl = el;
      const inRange = !this.selectable && i >= lo && i <= hi && row.type === "node";
      el.classList.toggle("in-range", inRange);
      if (!this.selectable) {
        if (inRange) el.setAttribute("aria-selected", "true");
        else el.removeAttribute("aria-selected");
      }
      els.set(row.key, el);
      fragment.push(el);
      if (row.type === "node") rendered.push(row.node.entry);
    }
    this.rowEls = els;
    this.windowEl.style.transform = `translateY(${start * h}px)`;
    this.windowEl.replaceChildren(...fragment);
    this.onRowsRendered(rendered);
  }

  /** Index bounds of the focused range (empty when there is none). */
  rangeBounds() {
    if (this.selectable || this.rangeAnchorKey == null) return [1, 0];
    const a = this.rowIndex.get(this.rangeAnchorKey);
    const b = this.activeIndex();
    if (a == null || b < 0) return [1, 0];
    return a <= b ? [a, b] : [b, a];
  }

  createRowEl(row) {
    const el = document.createElement("div");
    el.className = "row";
    el.id = `row-${row.key}`;
    el.setAttribute("role", "treeitem");
    el.dataset.key = row.key;
    el.innerHTML =
      '<span class="cell name"><span class="indent"></span><span class="twisty"></span>' +
      '<span class="mark"></span><span class="icon"></span><span class="label"></span></span>' +
      '<span class="cell size"></span><span class="cell date modified"></span><span class="cell date created"></span>';

    if (row.type === "node") {
      const { entry } = row.node;
      const [, size, modified, created] = el.children;
      el.querySelector(".label").textContent = entry.name;
      el.querySelector(".label").title = entry.path;
      el.querySelector(".mark").innerHTML = ICONS.check;
      el.querySelector(".icon").classList.add(entry.kind);
      el.querySelector(".icon").innerHTML = ICONS[entry.kind];
      if (isDir(row.node)) el.querySelector(".twisty").innerHTML = ICONS.twisty;
      if (entry.size != null) {
        size.textContent = formatSize(entry.size);
        size.title = formatBytesExact(entry.size);
      }
      modified.textContent = formatDate(entry.modified);
      modified.title = entry.modified ?? "";
      created.textContent = formatDate(entry.created);
      created.title = entry.created ?? "";
      this.decorateRow(el, entry);
    } else {
      el.classList.add("special", row.type);
    }
    return el;
  }

  updateRowEl(el, row) {
    el.style.setProperty("--depth", row.depth);
    el.setAttribute("aria-level", String(row.depth + 1));

    if (row.type === "node") {
      const { node } = row;
      const selected = this.looksSelected(node);
      el.classList.toggle("selected", selected);
      el.setAttribute("aria-selected", String(selected));
      if (isDir(node)) el.setAttribute("aria-expanded", String(node.expanded));
      el.setAttribute("aria-posinset", String(row.index + 1));
      el.setAttribute("aria-setsize", String(node.parent.total || node.parent.children.length));
      return;
    }

    const label = el.querySelector(".label");
    const icon = el.querySelector(".icon");
    const { node } = row;
    if (row.type === "loading") {
      icon.innerHTML = '<span class="spinner"></span>';
      label.textContent = "Loading…";
    } else if (row.type === "error") {
      icon.textContent = "";
      label.textContent = `${node.error} — press Space to retry`;
    } else {
      const remaining = node.total - node.nextOffset;
      icon.textContent = "";
      label.textContent = `Load ${formatCount(Math.min(remaining, this.pageSize))} more (${formatCount(remaining)} remaining)`;
    }
  }

  // ---- Active row ---------------------------------------------------------

  activeIndex() {
    return this.rowIndex.get(this.activeKey) ?? -1;
  }

  activeRow() {
    return this.rows[this.activeIndex()];
  }

  setActiveIndex(i) {
    this.showCursor();
    if (this.rows.length === 0) return;
    const clamped = Math.min(Math.max(i, 0), this.rows.length - 1);
    this.activeKey = this.rows[clamped].key;
    this.applyActive();
  }

  applyActive() {
    // Scroll the active row into view by position: it may not be rendered.
    const i = this.activeIndex();
    if (i >= 0) {
      const top = i * this.rowHeight;
      if (top < this.el.scrollTop) this.el.scrollTop = top;
      else if (top + this.rowHeight > this.el.scrollTop + this.el.clientHeight) {
        this.el.scrollTop = top + this.rowHeight - this.el.clientHeight;
      }
    }
    if (this.rangeAnchorKey != null || this.lastRanged) {
      this.dirty = true; // range classes change with the cursor
      this.lastRanged = this.rangeAnchorKey != null;
    }
    this.renderWindow(); // no-op unless the window moved or rows changed
    // Move the cursor highlight without rebuilding the window.
    if (this.activeEl && this.activeEl.dataset.key !== this.activeKey) this.activeEl.classList.remove("active");
    const el = this.rowEls.get(this.activeKey);
    el?.classList.add("active");
    this.activeEl = el ?? null;
    if (el) this.el.setAttribute("aria-activedescendant", el.id);
    else this.el.removeAttribute("aria-activedescendant");
    this.onActiveChange(this.activeRow() ?? null);
  }

  showCursor() {
    if (!this.cursorHidden) return;
    this.cursorHidden = false;
    this.el.classList.remove("cursor-hidden");
  }

  hideCursor() {
    this.cursorHidden = true;
    this.el.classList.add("cursor-hidden");
  }

  /**
   * Item rows of the focused range (non-selectable trees): from the range
   * anchor to the cursor, or just the cursor row without a range.
   */
  rangeRows() {
    const active = this.activeIndex();
    if (active < 0) return [];
    let from = this.rangeAnchorKey == null ? -1 : (this.rowIndex.get(this.rangeAnchorKey) ?? -1);
    if (from < 0) from = active;
    const [lo, hi] = from <= active ? [from, active] : [active, from];
    return this.rows.slice(lo, hi + 1).filter((r) => r.type === "node");
  }

  /** Re-renders the window so range classes follow the anchor. */
  applyRange() {
    this.dirty = true;
    this.renderWindow();
  }

  /** Entries of the rows currently in the DOM. */
  renderedEntries() {
    const out = [];
    for (let i = this.windowStart; i < this.windowEnd; i++) {
      const row = this.rows[i];
      if (row?.type === "node") out.push(row.node.entry);
    }
    return out;
  }

  /** Activate the row: toggle folders, load more, retry errors. */
  activate(row) {
    if (!row) return;
    if (row.type === "node" && isDir(row.node)) this.toggle(row.node);
    else if (row.type === "more") this.loadMore(row.node);
    else if (row.type === "error") this.load(row.node, { append: row.node.loaded });
  }

  visibleRowCount() {
    return Math.max(1, Math.floor(this.el.clientHeight / this.rowHeight) - 1);
  }

  // ---- Input --------------------------------------------------------------

  onKeyDown(e) {
    if (["Shift", "Control", "Alt", "Meta"].includes(e.key)) return; // modifiers alone (also auto-repeat)
    const i = this.activeIndex();
    const row = this.rows[i];

    if (this.selectable && e.key.toLowerCase() === "a" && (e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey) {
      e.preventDefault();
      this.selectAllVisible();
      return;
    }
    const moveTargets = {
      ArrowDown: i + 1,
      ArrowUp: i - 1,
      Home: 0,
      End: this.rows.length - 1,
      PageDown: i + this.visibleRowCount(),
      PageUp: i - this.visibleRowCount(),
    };
    // Selectable trees ignore Shift and Ctrl on cursor keys: they only move.
    const plainMove = this.selectable && e.key in moveTargets && !e.altKey;
    if ((e.ctrlKey || e.metaKey || e.altKey) && !plainMove) return;
    if (this.onKey(e, row)) {
      e.preventDefault();
      return;
    }
    if (this.selectable && e.key !== "Escape") this.showCursor();
    if (e.key in moveTargets) {
      e.preventDefault();
      this.moveCursor(moveTargets[e.key], e.shiftKey);
      return;
    }
    if (this.rangeAnchorKey != null) {
      // Any other key ends the focused range.
      this.rangeAnchorKey = null;
      this.applyRange();
    }

    let handled = true;
    switch (e.key) {
      case "ArrowRight":
        if (row?.type === "node" && isDir(row.node)) {
          if (!row.node.expanded) this.expand(row.node);
          else if (this.rows[i + 1]?.depth > row.depth) this.setActiveIndex(i + 1);
        }
        break;
      case "ArrowLeft":
        if (!row) break;
        if (row.type === "node" && row.node.expanded) {
          this.collapse(row.node);
        } else {
          const parent = row.type === "node" ? row.node.parent : row.node;
          if (parent && parent !== this.root) {
            this.activeKey = this.keyOf(parent);
            this.applyActive();
          }
        }
        break;
      case " ":
        // On "load more" / error rows Space loads or retries.
        if (row?.type !== "node") this.activate(row);
        else if (!this.selectable) handled = false;
        else this.toggleSelected(row);
        break;
      case "*":
        if (row?.type === "node") this.expandSiblings(row.node);
        break;
      case "r":
      case "R":
        // Mid-word "r" continues type-ahead; on its own it reloads.
        if (this.typeahead) handled = this.findByTyping(e.key, i);
        else this.refreshAround(row);
        break;
      case "Escape":
        this.typeahead = "";
        // Hide the cursor highlight first; with nothing highlighted, clear the selection.
        if (this.selectable) {
          if (!this.cursorHidden) this.hideCursor();
          else this.clearSelection();
        }
        break;
      default:
        handled = e.key.length === 1 && this.findByTyping(e.key, i);
    }
    if (handled) e.preventDefault();
  }

  /**
   * Moves the cursor. In non-selectable trees Shift focuses a range from the
   * row where it started; selectable trees ignore extend.
   */
  moveCursor(index, extend) {
    if (!this.selectable) {
      if (!extend) this.rangeAnchorKey = null;
      else if (this.rangeAnchorKey == null) this.rangeAnchorKey = this.activeKey;
    }
    this.setActiveIndex(index);
  }

  refreshAround(row) {
    if (!row) return this.refresh(this.root);
    let target = row.node;
    if (row.type === "node" && !(isDir(target) && target.expanded)) target = target.parent;
    this.refresh(target ?? this.root);
  }

  findByTyping(char, fromIndex) {
    clearTimeout(this.typeaheadTimer);
    this.typeaheadTimer = setTimeout(() => (this.typeahead = ""), TYPEAHEAD_RESET_MS);
    this.typeahead += char.toLowerCase();
    // A single character searches from the next row so repeated presses
    // cycle through matches; longer prefixes refine from the current row.
    const start = this.typeahead.length === 1 ? fromIndex + 1 : fromIndex;
    for (let k = 0; k < this.rows.length; k++) {
      const idx = (start + k + this.rows.length) % this.rows.length;
      const row = this.rows[idx];
      if (row.type === "node" && row.node.entry.name.toLowerCase().startsWith(this.typeahead)) {
        this.setActiveIndex(idx);
        return true;
      }
    }
    return true;
  }

  rowFromEvent(e) {
    const el = e.target.closest(".row");
    if (!el) return null;
    const index = this.rowIndex.get(el.dataset.key) ?? -1;
    return index < 0 ? null : { index, row: this.rows[index], el };
  }

  onClick(e) {
    const hit = this.rowFromEvent(e);
    if (!hit) return;
    this.el.focus({ preventScroll: true });
    if (!this.selectable) {
      if (!e.shiftKey) this.rangeAnchorKey = null;
      else if (this.rangeAnchorKey == null) this.rangeAnchorKey = this.activeKey;
    }
    this.setActiveIndex(hit.index);
    const { row } = hit;

    // Other clicks on item rows of selectable trees are drags (onMouseDown).
    if (row.type !== "node") this.activate(row);
    else if (this.selectable && e.target.closest(".mark")) this.toggleSelected(row);
    else if (e.target.closest(".twisty") && isDir(row.node)) this.activate(row);
  }

  // ---- Mouse drag (selectable trees) --------------------------------------

  /**
   * Mouse down on an item (not its checkbox or twisty) starts a drag over a
   * range of rows; a click is a drag over one row. Plain: the range becomes
   * the selection. Shift/Ctrl: the range is deselected if its first row was
   * selected, else selected; the rest of the selection stays.
   */
  onMouseDown(e) {
    if (e.button !== 0 || this.drag) return;
    const hit = this.rowFromEvent(e);
    if (hit?.row.type !== "node" || e.target.closest(".mark, .twisty")) return;
    e.preventDefault(); // no text selection; focus is set by hand
    this.el.focus({ preventScroll: true });
    const additive = e.shiftKey || e.ctrlKey || e.metaKey;
    this.drag = {
      fromKey: hit.row.key,
      toKey: hit.row.key,
      replace: !additive,
      select: !additive || !this.isSelected(hit.row.node),
      clientY: e.clientY,
    };
    this.setActiveIndex(hit.index);
    this.refreshMarks();

    const onMove = (ev) => {
      if (!this.drag) return;
      this.drag.clientY = ev.clientY;
      this.updateDrag();
    };
    const onKey = (ev) => {
      if (ev.key !== "Escape") return;
      // Esc while the button is down cancels; the mouse up then does nothing.
      ev.preventDefault();
      ev.stopPropagation();
      this.endDrag(false);
    };
    // Keeps scrolling while the mouse is held above or below the tree.
    const timer = setInterval(() => this.drag && this.updateDrag(), 50);
    const onUp = (ev) => {
      if (ev.button !== 0) return;
      clearInterval(timer);
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp, true);
      window.removeEventListener("keydown", onKey, true);
      this.endDrag(true);
    };
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp, true);
    window.addEventListener("keydown", onKey, true);
  }

  /** Moves the drag end (and cursor) to the row under the mouse; one row past an edge. */
  updateDrag() {
    if (this.rows.length === 0) return;
    const rect = this.el.getBoundingClientRect();
    const { clientY: y } = this.drag;
    const top = this.el.scrollTop;
    const height = this.el.clientHeight;
    let i;
    if (y < rect.top) i = Math.ceil(top / this.rowHeight) - 1;
    else if (y >= rect.top + height) i = Math.floor((top + height) / this.rowHeight);
    else i = Math.floor((y - rect.top + top) / this.rowHeight);
    i = Math.min(Math.max(i, 0), this.rows.length - 1);
    const { key } = this.rows[i];
    if (key === this.drag.toKey) return;
    this.drag.toKey = key;
    this.setActiveIndex(i);
    this.refreshMarks();
  }

  /** Index bounds of the drag range (empty without a drag). */
  dragBounds() {
    if (!this.drag) return [1, 0];
    const a = this.rowIndex.get(this.drag.fromKey);
    const b = this.rowIndex.get(this.drag.toKey);
    if (a == null || b == null) return [1, 0];
    return a <= b ? [a, b] : [b, a];
  }

  /** Ends the drag; commit applies the range to the selection, else it is dropped. */
  endDrag(commit) {
    const { drag } = this;
    if (!drag) return;
    const [lo, hi] = this.dragBounds();
    this.drag = null;
    if (commit) {
      if (drag.replace) this.selection.clear();
      for (const row of this.rows.slice(lo, hi + 1)) {
        if (row.type === "node") this.setSelected(row.node.entry, drag.select);
      }
    }
    this.refreshMarks();
    if (commit) this.onSelectionChange(this.selection);
  }

  onDoubleClick(e) {
    const hit = this.rowFromEvent(e);
    if (hit?.row.type === "node" && !e.target.closest(".twisty, .mark")) this.activate(hit.row);
  }
}
