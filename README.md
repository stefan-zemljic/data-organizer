# data_organizer

[![CI](https://github.com/stefan-zemljic/data-organizer/actions/workflows/ci.yml/badge.svg)](https://github.com/stefan-zemljic/data-organizer/actions/workflows/ci.yml)

Back up your disks, find what exists twice, and restore one clean copy.

data_organizer hashes whole folder trees (every file and every folder) and
keeps a compressed copy of every distinct file content. It shows which items
exist more than once, lets you exclude what you don't want, and restores the
rest (each item once, exactly as backed up) to a new place. It runs entirely
on your computer: one program with a browser UI, no account, no cloud.

![Browsing a backup of three old drives: whole folders that exist elsewhere are struck through](docs/images/use-cases/05-duplicate-folders.png)

**See it in action:** [Use cases](docs/use-cases.md) walks through merging an
old laptop disk, an external drive and a USB stick into one clean archive,
with screenshots (made-up data).

> **No warranty.** This is a hobby project, provided as is (see [LICENSE](LICENSE)).
> It reads and writes your files. Keep an independent backup of anything you
> can't afford to lose, and check restored data before deleting originals.

## Install

**Download:** get the archive for your system from
[Releases](https://github.com/stefan-zemljic/data-organizer/releases)
(Windows, Linux and macOS; amd64 and arm64), unpack it, and run
`data_organizer` (on Windows: `data_organizer.exe`).

**Or with Go 1.27+:**

```bash
go install github.com/stefan-zemljic/data-organizer@latest
```

(This installs the program as `data-organizer`.)

The program opens the UI in your browser. Stop it with Ctrl+C or by closing
its window; a running backup or restore is saved and continues on the next
start.

## Where your data goes

- `data_organizer.db` (the backups' file lists) and `data_organizer.packs/`
  (the stored file contents) are created in the **current folder**. When you
  double-click the program, that is the program's folder. Choose another place
  with `-db`, e.g. `data_organizer -db D:\backups\data_organizer.db`; the packs
  go next to it.
- Plan free space of roughly half the backed-up size on that drive (less with
  many duplicates). Each distinct content is stored once, compressed.
- Back up to a different disk than the one you back up, or the backup dies
  with the disk.

## Using it

1. **New backup**: pick folders or drives in the tree (arrow keys, Space to select), then Start backup. A backup first finds all files, then hashes and stores them.
2. **Progress**: one bar with the time left. Pause, resume or stop at any time. Closing the program (or a crash) keeps the backup: it continues on the next start.
3. **Backups**: all backups; Browse a completed one, or delete a backup with everything derived from it (files on disk stay).
4. **Backup**: the backed-up tree. Later copies of the same content are struck through. Press Delete to exclude an item and everything below it.
5. **Restore…**: restores everything not excluded, each item once, from the stored contents into an empty folder; the backed-up disks are not read again. The **Restores** tab shows progress and history; Browse a restore to see what was restored and what failed.

A full example with screenshots: [Use cases](docs/use-cases.md).

## Options

| Option | Default | Meaning |
|---|---|---|
| `-db` | `data_organizer.db` | Database file; the content store goes next to it. |
| `-addr` | `127.0.0.1:8080` | Listen address. Keep it on loopback. |
| `-open` | `true` | Open the UI in the default browser. |
| `-version` | | Print the version and exit. |

## Security

The UI can read your whole filesystem, so it only listens on `127.0.0.1` and
every request needs a secret access key. At startup the program prints a link
containing the key and opens your browser with a one-time code; after that the
browser keeps a cookie. Other programs and users on the same computer can't use
the UI without the key. Report vulnerabilities as described in
[SECURITY.md](SECURITY.md).

## Development

You need [Go 1.27+](https://go.dev/dl/) and, on Windows, Git Bash.

```bash
./dev
```

That shows a menu. Or name the command directly:

| Command | What it does |
|---|---|
| `./dev run` | Build and start the app in development mode: UI files are served from disk (edit, reload). Ctrl+C stops it. |
| `./dev test` | Run all tests. |
| `./dev check` | Everything before a commit: format, vet, tests. |
| `./dev platforms` | Tests on 32-bit Windows and on Linux 64/32-bit (in Docker). |
| `./dev build` | Build one program file with the UI embedded. |

```
main.go            startup, flags, shutdown
internal/
  fsbrowse/        live folder listings for the UI
  scan/            scanner: parallel walk + hashing (hash format in hash.go)
  content/         content store: chunking, compression, pack files, compaction
  backupjob/       runs one backup at a time (the scanner, storing, resume)
  store/           SQLite: schema (migrations/), backups, entries, exclusions, restores, content index
  restorejob/      runs one restore at a time
  server/          HTTP routes (Echo) and the access key
web/static/        UI: plain JavaScript modules, no build step
docs/              architecture.md: data flow, hash format, API, database, security
```

Rules:

- The database and the content store hold your backups; never commit them. A git hook refuses them; `./dev` enables it.
- Schema changes are new files in `internal/store/migrations/`. Never edit an existing one.
- Releases: push a tag `vX.Y.Z`; GitHub Actions builds the archives with GoReleaser.

## License

[MIT](LICENSE): use, change and share it freely; no warranty, no liability.
