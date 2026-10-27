# Architecture

How data_organizer works inside. Start with the [README](../README.md).

## Overview

One Go program serves a JSON API and the browser UI on loopback
(`127.0.0.1:8080`) and keeps backups in one SQLite file (`data_organizer.db`), plus
a content store with every distinct file content (`data_organizer.packs/`).
The browser never touches the filesystem; it only calls the API.

Terms: a **backup** scans folders (the scanner in `internal/scan` walks and hashes) and stores every distinct file content; its **entries** are the backed-up items you browse and exclude; a **restore** writes the kept entries of a backup into a folder. Duplicates are still called copies ("first copy", "later copy"). Until 2026-10-26 backups were called scans, entries results and restores copies.

```
browser (web/static, plain ES modules)
   │ HTTP/JSON, polled
internal/server ── routes, security middleware, error → status mapping
   ├─ internal/fsbrowse ── live folder listings (New backup, folder picker)
   ├─ internal/backupjob ─ one backup at a time ── internal/scan (walk + hash)
   │                                       └─ internal/content (store contents)
   ├─ internal/restorejob ─ one restore at a time ── internal/content (read)
   └─ internal/store ───── SQLite: backups, items, exclusions, duplicates, restores,
                            content index
```

| Path | What it does |
|---|---|
| `main.go` | Flags (`-addr`, `-db`, `-dev`), startup housekeeping (compact the content store, continue an unfinished backup or restore), graceful shutdown (a running backup is paused and saved for the next start; a running restore is saved and continues on the next start). |
| `internal/fsbrowse` | `Roots()`, `List(path, offset, limit)`: one folder per call, sorted, paged, only regular files and real folders. |
| `internal/scan` | `Scanner`, the engine of a backup: parallel walk and hash (`hash.go` is the hash format), `Item`/`Progress`, `NormalizeRoots`. |
| `internal/backupjob` | `Manager`: start, pause/resume, stop, recover after a restart; streams scanner items into the store; duplicate analysis when done. |
| `internal/content` | The content store's bytes: `Writer` (chunk, dedup, compress, append to packs), `Reader`, `Rewrite`/`RemovePacks` for compaction. |
| `internal/store` | Everything SQLite: schema (`migrations/`), item writer, duplicate analysis (`analyze.go`), browsing and exclusions (`entries.go`, `exclusions.go`), restores (`restores.go`), content index and compaction (`contents.go`). |
| `internal/restorejob` | `Manager`: start, pause/resume, stop, recover after a restart; restores what a backup shows as kept (not excluded, first copy) from the content store into a target folder (`plan.go`: what; `manager.go`: walk, workers, restore). |
| `internal/server` | Echo v5 wiring (`server.go`), backup routes (`backups.go`), restore routes (`restores.go`). |
| `web/` | `embed.go` embeds `static/`. `app.js` (tabs, poller), one module per view (`wizard` = New backup, `progress`, `history` = Backups, `backup`, `restores`, `restore`), shared `tree.js` (keyboard tree with virtual scrolling), `api.js`, `eta.js`, `format.js`, `confirm.js`. |

Backups and restores never run at the same time (both work the disks hard).

## Hash format

Changing it changes every stored hash.

- file = `sha256(0x00 ‖ content)`
- dir = `sha256(0x01 ‖ XOR of its entries)`, entry = `sha256(u64le(len(name)) ‖ name ‖ childHash)`, name = UTF-8 item name (not the path). XOR makes the order of children irrelevant.
- Unreadable items get an error and are left out of their parent's hash and counts; the parent is not flagged. Every empty folder has the same hash, so empty folders are duplicates of each other.

## Backing up

1. **Find** (discovery): up to 64 goroutines list folders (unsorted `ReadDir`, sizes and times from the listing, free on Windows) and queue all files. Totals are known afterwards: C:\ with 2.24M files takes about 9 s.
2. **Hash and store**: `2×CPU` (4..32) workers read files once, hash them and store their contents (see Content store). Each folder counts its unfinished children; the last child to finish computes the folder hash and passes it up. Items are emitted when complete (children before parents).
3. **Analyze**: after the last item is written, duplicates are found (status `analyzing`).

- Item ids are assigned by the scanner from `MAX(id)+1` (safe, one backup at a time), so children know their parent id before the parent row exists, and a backup's ids are one contiguous range.
- Pause and Stop make workers exit soon; a file mid-hash goes back into the queue. Resume restarts workers on the in-memory queues.

### Surviving restarts and crashes

An unfinished backup continues when the program starts again, whether it was shut down (Ctrl+C pauses the backup and saves it) or killed. At most the last unsaved batch (about a second) is read again.

- Every folder is recorded the moment it is found (`scan.Item.Listed` → table `backup_dirs`: id, parent, name), before anything inside it, through the same ordered writer as the items.
- Items are sent only when complete (children before parents) and committed in order, so whatever was committed is consistent: a stored folder has all its children stored; every stored item's parent is stored or recorded.
- On start, `backupjob.Manager.Recover` takes the newest backup if it is still `running` or `paused` (`store.RecoverBackup`). `store.LoadPartial` reads its unfinished folders (recorded, no item yet), finished roots and stored counters; `scan.Resume` lists only the unfinished folders again. They keep their ids, so stored children stay attached, are folded into the folder hash from the database (`Children`), and only the files not stored yet are hashed. Folders that appeared since are scanned normally; recorded folders that vanished are reported as unreadable.
- A running backup continues by itself; a paused one waits for Resume or Stop (and blocks new backups and restores, as before). A crash during duplicate analysis redoes only the analysis.
- `backup_dirs` rows of a backup are dropped when it ends. Backups without them (from before this, `backups.resumable = 0`) and active backups that are not the newest (their id range must stay contiguous) still become `interrupted`.
- The writer stores items in batches (10k per transaction, flushed every second). A database error stops the backup as `failed`.

## Duplicates and exclusions

- `AnalyzeDuplicates` (`analyze.go`, in Go rather than SQL: about 5 s instead of 4 min for C:\) groups a backup's items by hash and ranks the copies in each group by path (`dup_rank`, 1 = first; siblings ordered by lowercase name, then name).
- A copy is shown struck through ("later copy") when a copy with a lower rank exists that is not excluded.
- Exclusion stores only the explicit mark on the item (`items.excluded`), so marking is O(1) even for C:\. "Excluded" (the item or an ancestor is marked) is derived when reading (`exclusionResolver`). Excluding never affects other copies.

## Content store

Every backup stores what it reads, so a restore never reads the backed-up disks again (`internal/content`, index in `store/contents.go`).

- Chunks: files are cut into content-defined chunks (Rabin, `restic/chunker`, 512 KiB..8 MiB, about 1 MiB on average; the polynomial must never change). A changed large file shares most chunks with its earlier version; a file under 512 KiB is one chunk.
- Chunk hash = the file hash formula over the chunk, so a one-chunk file needs a single SHA-256 pass.
- Dedup: the writer loads every stored chunk hash and content hash at backup start (memory: about 60 bytes per chunk). A chunk already stored is neither compressed nor written, so duplicates and unchanged files cost only the read and hash.
- Compression: zstd fastest level, no frame checksum (restores check the file hash). Chunks of 128 KiB or more are first probed on 64 KiB: under 3 % gain means stored raw (media, archives) without compressing the rest.
- Pack files: chunks are appended to `NNNNNNNN.pack` files (closed at 64 MiB) in `<db name>.packs/`. Packs are synced before each item batch commits its index rows, so a crash leaves at most unindexed bytes.
- Index: `chunks(id, hash, pack, offset, stored, size, codec)` and `contents(hash = items.hash, size, chunks)` with the chunk ids as varint deltas (consecutive ids take 1 byte).
- Deleting a backup drops contents no file item uses anymore and chunks no content uses, then compacts: packs without live chunks are deleted, packs under 50 % live are rewritten (live chunks copied to new packs, index updated, old packs deleted). Rewriting never costs more than it frees. Startup runs the same compaction, which also removes crash leftovers.

## Restoring

- What: readable items that are not excluded and are the first copy by path among the non-excluded copies (exactly what the Backup view shows yellow and not struck through).
- Where: the target folder must be new or empty. `C:\Users\x` is copied to `<target>\C\Users\x`. Files keep their modification time.
- How: depth-first over folders in batches, 4 file workers; each file is assembled from its stored chunks, hashed and compared with the backup (catches a damaged store). Outcome per file: restored or failed (not written, or stored content damaged; a failed file is removed). Folders are flagged when something below them failed. The source disks are not read.

### Pause, restarts and crashes

A restore can be paused and resumed, and an unfinished one continues when the program starts again, whether it was shut down (Ctrl+C) or killed.

- Every file's outcome is a `restore_items` row, committed by one writer in batches (10k, flushed every second). The recorded rows are the restore's state; nothing else is needed to continue.
- Pause ends the current run (a file mid-write is removed and not recorded); the folder rows are written as on Stop, so a paused restore can be browsed. The job stays active while paused: no run, but backups, backup deletion and new restores stay blocked.
- Resume, and the next start (`restorejob.Manager.Recover` takes the newest `running` or `paused` restore via `store.RecoverRestore`; a running one continues by itself, a paused one waits), start a new run: the plan is made again, the outcomes are loaded (`store.RestoreOutcomes`), the counters start from them, and the walk skips every file with a recorded outcome (failed files are not retried). Other files are written again; a file already at the destination is a leftover of the earlier run (written after the last commit, or partly) and is replaced. At most about a second of files is written twice after a crash.
- Because a resumed run plans again, a backup's exclusions cannot change while one of its restores is running or paused (`store.ErrRestoreUnfinished`, 409).
- Restores interrupted before this existed (status `interrupted`) stay as they are.

## API

JSON over HTTP; errors are `{message}` with a fitting status (400 bad input, 404, 409 busy or not possible now).

- Browse: `GET /api/roots` → `{entries}`; `GET /api/list?path=&offset=&limit=1..1000` → `{path, offset, limit, total, nextOffset, entries}`. `Entry {name, path, kind: "dir"|"file", size? (files only), created?, modified}`.
- Backups: `GET /api/backups` (newest first), `POST /api/backups {roots}`, `GET /api/backups/current` → `{backup|null}`, `GET /api/backups/:id`, `POST /api/backups/:id/pause|resume|stop`, `DELETE /api/backups/:id` → `{deleted: {items, restores, names, contents, bytesFreed}}` (409 while a backup or restore runs).
  `Backup {id, status, startedAt, finishedAt?, roots, progress {discovering, dirsFound, dirsDone, filesFound, filesDone, bytesFound, bytesDone, errors}, error?, ready}` (`ready`: completed and analyzed, so it can be browsed and restored).
  Statuses: `running`, `paused`, `completed`, `stopped`, `failed`, `interrupted`, and while active also `pausing`, `stopping`, `analyzing`.
- Backup entries (ready backups only, else 409): `GET /api/backups/:id/entries/roots`, `GET /api/backups/:id/entries/children?parent=<itemId>&offset&limit`. `BackupEntry {id, name, kind, size?, modified?, error?, dupCount, excludedSelf, excluded, notFirst}`. Exclusion: `POST …/entries/exclude {items, on}`, `POST …/entries/reset` → `{removed}`, `POST …/entries/state {items}` → fresh entries.
- Restores: `POST /api/backups/:id/restore {target}`, `GET /api/restores`, `GET /api/restores/current` → `{restore|null}`, `GET /api/restores/:id`, `POST /api/restores/:id/pause|resume|stop`, `GET /api/restores/:id/entries/roots|children`. `Restore {id, backupId, target, status, startedAt, finishedAt?, progress {filesTotal, bytesTotal, filesDone, bytesDone, filesOk, filesFailed, dirsCreated}, error?}`.
  Statuses: `running`, `paused`, `completed`, `stopped`, `failed`, `interrupted`, and while active also `pausing`, `stopping`. `GET /api/restores/current` returns a running or paused restore.

## Database

- `backups`, `backup_roots`, `names` (every distinct file name once, shared by all backups), `items(id, backup_id, parent_id, name_id, kind, size, mtime, hash, error, dup_count, dup_rank, excluded)`, `backup_dirs` (folders of an unfinished backup, see Surviving restarts), `restores`, `restore_items`, the content index `chunks`, `contents` (see Content store), and `settings` (the access key, see Security). Until migration 00003 (2026-10-26) these were `scans`, `scan_roots`, `scan_dirs`, `copies`, `copy_items` with `scan_id`/`copy_id`; index names (`items_scan_roots`, `scan_dirs_scan`) still carry the old words, since SQLite cannot rename an index without rebuilding it.
- A path is rebuilt by walking `parent_id` up; root items have no parent and their full path as name.
- A backup's items are the id range from `backups.first_item_id` to the next backup's (`backupRange`); stored because a stopped backup may have no root item. Analysis, restore planning and deletion read by this range (the primary key).
- Deleting a backup (`DeleteBackup`, one transaction, then compaction) removes its items, its restores' records, names and file contents no other backup uses; files on disk are never touched. C: (2.24M items) took about 11 s before the content store existed (not yet measured with it). The database file does not shrink; SQLite reuses the freed pages.
- Connection: WAL, `synchronous=NORMAL`, 256 MB cache, temp tables in memory (`store.Open`).
- Every per-backup query on `items` must use an index-backed condition (all indexes are partial or by parent); otherwise it reads every backup's rows.
- Schema changes: add a new numbered file in `internal/store/migrations/` (goose, applied at startup); never edit an applied one. The 12 early migrations were squashed into `00001_init.sql` when the database was reset for the content store (2026-10-26).
- The database and the packs are never committed: `.gitignore` plus `.githooks/pre-commit` (enabled by `./dev`).

## Security

The API can read the whole filesystem, so:

- It listens on loopback only.
- Every request needs the access key (`server.Access`): a random secret kept in the `settings` table, so browser sessions survive restarts. Startup prints a link with `?key=`; the browser is opened with a one-time `?code=` (valid one minute) instead, so the key never shows up in a process command line. Either sets an HttpOnly, SameSite=Strict cookie named per port and redirects to the URL without the secret. Other programs and users on the machine can reach the port, but get 401 without the key.
- A Host header allowlist blocks DNS rebinding.
- Cross-site POSTs are rejected (`http.CrossOriginProtection`), and request bodies are capped at 8 MB.
- A strict CSP applies (`default-src 'self'`), plus nosniff and frame denial.
- API responses are never cached; UI files are revalidated on every load.

## UI notes

- No build step: edit a file in `web/static`, reload the page (`./dev run` serves it from disk).
- `tree.js` is the keyboard-driven tree used by New backup, Backup, Restore and the folder picker. It only renders visible rows (virtual scrolling), so tests must navigate with keys, not query far rows.
- Selection (New backup only; the other trees are `selectable: false` and keep Shift+cursor/click focused ranges): a set of paths, independent of what is loaded or expanded. Items inside a selected folder look selected too; `looksSelected` derives this from the loaded ancestors, never by reading the folder. Arrows only move the cursor (Shift/Ctrl ignored); Space and checkbox clicks toggle. Mouse down on a row starts a drag (a click is a one-row drag), previewed live: plain replaces the selection with the range on mouse up; Shift/Ctrl (shown in the footer: Shift only) deselects the range if its first row was selected, else selects it, keeping the rest. Esc during a drag cancels it (the mouse up then does nothing); otherwise Esc hides the cursor highlight, and a second Esc clears the selection.
- One poller in `app.js`: every 0.5 s while a backup or restore is active, every 3 s otherwise. A failed request shows the "Backend not available" page until the backend answers again.
- Progress bars: bytes done of the known total; the time left is the larger of the byte-rate and file-rate estimates over the last minute (`eta.js`).

## Known limitations

- A backup stores everything it reads, including what is excluded later (exclusions are made afterwards). The store needs free space of roughly the compressed, deduplicated backup size on the database's drive.
- Each file is restored with one index query (fine for millions of files, not tuned).
- Live folder listings are cached for up to 30 s (refreshed when the folder itself changes), so file sizes on later pages can lag behind.
- Type-ahead only searches loaded rows. The New backup selection lives in the page and is lost on reload.
