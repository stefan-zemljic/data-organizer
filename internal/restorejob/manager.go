// Package restorejob restores a backup into a target folder from the
// content store: every item that is not excluded and is the first copy by
// path, exactly as backed up (the source disks are not read). Each restored
// file is hashed and compared with the backup, which catches a damaged
// store. One restore runs at a time; it can be paused, and a restore the
// program left unfinished (shut down or killed) continues on the next start.
package restorejob

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stefan-zemljic/data-organizer/internal/content"
	"github.com/stefan-zemljic/data-organizer/internal/store"
)

// Transient statuses, only seen while a restore is active.
const (
	StatusPausing  = "pausing"
	StatusStopping = "stopping"
)

var (
	ErrBusy           = errors.New("another restore is running")
	ErrBackupBusy     = errors.New("a backup is running or being deleted; restore when that is done")
	ErrNotActive      = errors.New("restore is not active")
	ErrInvalidState   = errors.New("action not possible in the current restore state")
	ErrInvalidTarget  = errors.New("invalid target folder")
	ErrTargetNotEmpty = errors.New("the target folder must be new or empty")
)

const (
	walkBatch     = 500
	flushInterval = time.Second // outcomes are committed at least this often
)

type Manager struct {
	store      *store.Store
	backupBusy func() bool
	workers    int

	mu     sync.Mutex
	active *job
}

// New returns a manager; backupBusy reports whether a backup is active or
// being deleted.
func New(st *store.Store, backupBusy func() bool) *Manager {
	return &Manager{store: st, backupBusy: backupBusy, workers: 4}
}

// job is the active restore. It stays active while paused, with no run
// going (done closed).
type job struct {
	id       int64
	backupID int64
	target   string
	started  time.Time

	mu      sync.Mutex
	status  string
	leaving bool               // the program exits: save and keep the restore unfinished
	cancel  context.CancelFunc // ends the current run
	done    chan struct{}      // closed when the current run has saved its outcome

	filesTotal, bytesTotal atomic.Int64
	filesDone, bytesDone   atomic.Int64
	filesOK, filesFailed   atomic.Int64
	dirsCreated            atomic.Int64
}

// Busy reports whether a restore is running or paused (backups must not start meanwhile).
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active != nil
}

// Start begins restoring a completed backup into target.
func (m *Manager) Start(ctx context.Context, backupID int64, target string) (store.Restore, error) {
	backup, err := m.store.GetBackup(ctx, backupID)
	if err != nil {
		return store.Restore{}, err
	}
	if !backup.Ready {
		return store.Restore{}, store.ErrNotReady
	}
	target, err = prepareTarget(target)
	if err != nil {
		return store.Restore{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		return store.Restore{}, ErrBusy
	}
	if m.backupBusy() {
		return store.Restore{}, ErrBackupBusy
	}
	j := &job{backupID: backupID, target: target, started: time.Now(), status: store.RestoreRunning}
	if j.id, err = m.store.CreateRestore(ctx, backupID, target, j.started); err != nil {
		return store.Restore{}, err
	}
	m.active = j
	j.mu.Lock()
	m.launch(j, false)
	j.mu.Unlock()
	return j.snapshot(), nil
}

// Recover picks up the restore that was unfinished when the program last
// exited (shut down or killed): a running one continues, a paused one waits
// for Resume or Stop. Files with a recorded outcome are not written again.
// Call it once, after the backup manager's Recover and before serving.
func (m *Manager) Recover(ctx context.Context) error {
	r, ok, err := m.store.RecoverRestore(ctx)
	if err != nil || !ok {
		return err
	}
	if m.backupBusy() { // not possible through the API: they exclude each other
		return m.store.FinishRestore(ctx, r.ID, store.RestoreInterrupted, r.Progress, time.Now(), "a backup was unfinished as well")
	}
	j := &job{id: r.ID, backupID: r.BackupID, target: r.Target, started: r.StartedAt, status: r.Status}
	j.setProgress(r.Progress)
	slog.Info("resuming restore", "restore", r.ID, "status", r.Status)
	m.mu.Lock()
	m.active = j
	m.mu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	if r.Status == store.RestorePaused {
		j.cancel = func() {}
		j.done = make(chan struct{})
		close(j.done)
		return nil
	}
	m.launch(j, true)
	return nil
}

// launch starts a run of the job; resumed skips what earlier runs recorded.
// Call with j.mu held.
func (m *Manager) launch(j *job, resumed bool) {
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	j.done = make(chan struct{})
	go m.run(ctx, j, resumed)
}

// prepareTarget checks that target is an absolute path to a new or empty
// folder and creates it.
func prepareTarget(target string) (string, error) {
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("%w: the path must be absolute", ErrInvalidTarget)
	}
	target = filepath.Clean(target)
	info, err := os.Stat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(target, 0o755); err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalidTarget, err)
		}
		return target, nil
	case err != nil:
		return "", fmt.Errorf("%w: %v", ErrInvalidTarget, err)
	case !info.IsDir():
		return "", fmt.Errorf("%w: not a folder", ErrInvalidTarget)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidTarget, err)
	}
	if len(entries) > 0 {
		return "", ErrTargetNotEmpty
	}
	return target, nil
}

func (m *Manager) activeJob(id int64) (*job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil || m.active.id != id {
		return nil, ErrNotActive
	}
	return m.active, nil
}

// Pause ends the current run soon; what was restored stays recorded and
// Resume continues from there.
func (m *Manager) Pause(id int64) (store.Restore, error) {
	j, err := m.activeJob(id)
	if err != nil {
		return store.Restore{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status != store.RestoreRunning {
		return store.Restore{}, ErrInvalidState
	}
	j.status = StatusPausing
	j.cancel()
	return j.snapshotLocked(), nil
}

// Resume continues a paused restore.
func (m *Manager) Resume(ctx context.Context, id int64) (store.Restore, error) {
	j, err := m.activeJob(id)
	if err != nil {
		return store.Restore{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.status != store.RestorePaused || j.leaving {
		return store.Restore{}, ErrInvalidState
	}
	if err := m.store.SaveRestoreProgress(ctx, j.id, store.RestoreRunning, j.progress()); err != nil {
		return store.Restore{}, err
	}
	j.status = store.RestoreRunning
	m.launch(j, true)
	return j.snapshotLocked(), nil
}

// Stop ends the restore (soon, when running); what was restored stays.
func (m *Manager) Stop(ctx context.Context, id int64) (store.Restore, error) {
	j, err := m.activeJob(id)
	if err != nil {
		return store.Restore{}, err
	}
	j.mu.Lock()
	switch j.status {
	case store.RestoreRunning, StatusPausing:
		j.status = StatusStopping
		j.cancel()
		r := j.snapshotLocked()
		j.mu.Unlock()
		return r, nil
	case store.RestorePaused: // no run to end
		j.status = store.RestoreStopped
		j.mu.Unlock()
		err := m.store.FinishRestore(ctx, j.id, store.RestoreStopped, j.progress(), time.Now(), "")
		m.mu.Lock()
		m.active = nil
		m.mu.Unlock()
		if err != nil {
			return store.Restore{}, err
		}
		return m.store.GetRestore(ctx, j.id)
	}
	j.mu.Unlock()
	return store.Restore{}, ErrInvalidState
}

// Current returns the running or paused restore, if any.
func (m *Manager) Current() (store.Restore, bool) {
	m.mu.Lock()
	j := m.active
	m.mu.Unlock()
	if j == nil {
		return store.Restore{}, false
	}
	return j.snapshot(), true
}

// List returns all restores, newest first, with live data for the running one.
func (m *Manager) List(ctx context.Context) ([]store.Restore, error) {
	restores, err := m.store.ListRestores(ctx)
	if err != nil {
		return nil, err
	}
	if cur, ok := m.Current(); ok {
		for i := range restores {
			if restores[i].ID == cur.ID {
				restores[i] = cur
			}
		}
	}
	return restores, nil
}

func (m *Manager) Get(ctx context.Context, id int64) (store.Restore, error) {
	if cur, ok := m.Current(); ok && cur.ID == id {
		return cur, nil
	}
	return m.store.GetRestore(ctx, id)
}

// Shutdown ends a running restore and waits until its progress is saved;
// it stays unfinished and the next start continues it (see Recover).
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	j := m.active
	m.mu.Unlock()
	if j == nil {
		return
	}
	j.mu.Lock()
	j.leaving = true
	j.cancel()
	done := j.done
	j.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (j *job) progress() store.RestoreProgress {
	return store.RestoreProgress{
		FilesTotal:  j.filesTotal.Load(),
		BytesTotal:  j.bytesTotal.Load(),
		FilesDone:   j.filesDone.Load(),
		BytesDone:   j.bytesDone.Load(),
		FilesOK:     j.filesOK.Load(),
		FilesFailed: j.filesFailed.Load(),
		DirsCreated: j.dirsCreated.Load(),
	}
}

func (j *job) setProgress(p store.RestoreProgress) {
	j.filesTotal.Store(p.FilesTotal)
	j.bytesTotal.Store(p.BytesTotal)
	j.filesDone.Store(p.FilesDone)
	j.bytesDone.Store(p.BytesDone)
	j.filesOK.Store(p.FilesOK)
	j.filesFailed.Store(p.FilesFailed)
	j.dirsCreated.Store(p.DirsCreated)
}

func (j *job) snapshot() store.Restore {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snapshotLocked()
}

func (j *job) snapshotLocked() store.Restore {
	return store.Restore{ID: j.id, BackupID: j.backupID, Target: j.target, Status: j.status, StartedAt: j.started, Progress: j.progress()}
}

type fileTask struct {
	index   int
	id      int64
	dst     string
	hash    []byte
	modTime int64
	replace bool // a resumed run: a file at dst is a leftover of an earlier run
}

type dirNode struct {
	id  int64
	dst string
}

// run plans, walks and restores, then records the outcome: finished, or
// paused (the job stays active), or unfinished because the program exits.
func (m *Manager) run(ctx context.Context, j *job, resumed bool) {
	err := m.restoreAll(ctx, j, resumed)
	bg := context.Background()
	// Held until the outcome is saved, so Pause, Resume and Stop see either
	// the run or its saved result.
	j.mu.Lock()
	final, errMsg := store.RestoreCompleted, ""
	switch {
	case ctx.Err() == nil && err != nil:
		final, errMsg = store.RestoreFailed, err.Error()
	case ctx.Err() == nil:
	case j.status == StatusStopping:
		final = store.RestoreStopped
	case j.status == StatusPausing:
		final = store.RestorePaused
	default: // the program exits while running
		final = store.RestoreRunning
	}
	switch final {
	case store.RestorePaused, store.RestoreRunning:
		if err := m.store.SaveRestoreProgress(bg, j.id, final, j.progress()); err != nil {
			slog.Error("saving restore progress", "restore", j.id, "err", err)
		}
	default:
		if err := m.store.FinishRestore(bg, j.id, final, j.progress(), time.Now(), errMsg); err != nil {
			slog.Error("saving restore result", "restore", j.id, "err", err)
		}
	}
	j.cancel()
	j.status = final
	keep := final == store.RestorePaused && !j.leaving
	close(j.done)
	j.mu.Unlock()
	if !keep {
		m.mu.Lock()
		m.active = nil
		m.mu.Unlock()
	}
}

// restoreAll plans the restore, walks the backup while workers restore the files, and
// records the outcome of every file and folder. A resumed run starts from
// the outcomes earlier runs recorded and restores only the other files.
func (m *Manager) restoreAll(ctx context.Context, j *job, resumed bool) error {
	p, err := makePlan(ctx, m.store, j.backupID)
	if err != nil {
		return err
	}
	rd, err := m.store.NewContentReader()
	if err != nil {
		return err
	}
	defer rd.Close()
	n := len(p.base.Parent)
	status := make([]uint8, n) // outcome per item; directories: 0 = not created
	var statusMu sync.Mutex    // status is written by workers and the walker

	// Counters start from what is recorded (dirsCreated counts again: the
	// walk sees every folder again, created or found).
	var prog store.RestoreProgress
	prog.FilesTotal, prog.BytesTotal = p.totals()
	var recorded []bool // files with an outcome from an earlier run: skipped
	if resumed {
		recorded = make([]bool, n)
		err := m.store.RestoreOutcomes(ctx, j.id, func(id int64, st int) {
			i := p.index(id)
			if i < 0 {
				return
			}
			status[i] = uint8(st)
			if p.base.IsDir[i] {
				return
			}
			recorded[i] = true
			prog.FilesDone++
			switch st {
			case store.RestoreItemOK:
				prog.FilesOK++
				prog.BytesDone += p.base.Size[i]
			case store.RestoreItemFailed:
				prog.FilesFailed++
			}
		})
		if err != nil {
			return err
		}
	}
	j.setProgress(prog)

	// Outcomes go to the database through one writer goroutine.
	out := make(chan store.RestoreOutcome, 4096)
	writerDone := make(chan error, 1)
	go func() {
		w := m.store.NewRestoreItemWriter(j.id)
		ticker := time.NewTicker(flushInterval) // a crash loses at most this much
		defer ticker.Stop()
		var werr error
		for {
			select {
			case o, ok := <-out:
				if !ok {
					if werr == nil {
						werr = w.Flush()
					}
					writerDone <- werr
					return
				}
				if werr == nil {
					werr = w.Write(context.Background(), o)
				}
			case <-ticker.C:
				if werr == nil {
					werr = w.Flush()
				}
			}
		}
	}()

	record := func(i int, id int64, st int, msg string) {
		statusMu.Lock()
		status[i] = uint8(st)
		statusMu.Unlock()
		out <- store.RestoreOutcome{ItemID: id, Status: st, Err: msg}
	}

	var made sync.Map // directories already created
	ensureDir := func(dir string) error {
		if _, ok := made.Load(dir); ok {
			return nil
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if _, loaded := made.LoadOrStore(dir, true); !loaded {
			j.dirsCreated.Add(1)
		}
		return nil
	}

	tasks := make(chan fileTask, 256)
	var wg sync.WaitGroup
	for range m.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var bufs buffers
			for t := range tasks {
				if ctx.Err() != nil {
					continue // drain
				}
				st, msg := m.restoreFile(ctx, j, rd, t, &bufs, ensureDir)
				if st == 0 || (st == store.RestoreItemFailed && ctx.Err() != nil) {
					continue // canceled mid-file: nothing recorded, the next run restores it
				}
				j.filesDone.Add(1)
				switch st {
				case store.RestoreItemOK:
					j.filesOK.Add(1)
				case store.RestoreItemFailed:
					j.filesFailed.Add(1)
				}
				record(t.index, t.id, st, msg)
			}
		}()
	}

	walkErr := m.walk(ctx, j, p, tasks, recorded, ensureDir, func(i int, d dirNode) {
		// A directory that is restored itself is created even when empty.
		if err := ensureDir(d.dst); err != nil {
			record(i, d.id, store.RestoreItemFailed, shortError(err))
			return
		}
		statusMu.Lock()
		status[i] = store.RestoreItemOK
		statusMu.Unlock()
	})
	close(tasks)
	wg.Wait()

	folderOutcomes(p, status, out)
	close(out)
	if err := <-writerDone; err != nil {
		return err
	}
	return walkErr
}

// folderOutcomes sends one row for every created folder and every ancestor
// of a recorded item, flagged when errors or differences lie below it.
// status holds the outcome per item (0 = nothing recorded).
func folderOutcomes(p *plan, status []uint8, out chan<- store.RestoreOutcome) {
	n := len(status)
	flags := make([]uint8, n)
	needRow := make([]bool, n)
	for i := 0; i < n; i++ {
		if status[i] == 0 {
			continue
		}
		var f uint8
		switch status[i] {
		case store.RestoreItemFailed:
			f = store.RestoreFlagErrorsBelow
		}
		for cur := p.index(p.base.Parent[i]); cur >= 0; cur = p.index(p.base.Parent[cur]) {
			if needRow[cur] && flags[cur]&f == f {
				break // the rest of the chain is marked already
			}
			needRow[cur] = true
			flags[cur] |= f
		}
	}
	for i := 0; i < n; i++ {
		if !p.base.Exists[i] || !p.base.IsDir[i] || (!needRow[i] && status[i] == 0) {
			continue
		}
		st := int(status[i])
		if st == 0 {
			st = store.RestoreItemOK // created on the way to a restored file
		}
		if st == store.RestoreItemFailed && flags[i] == 0 {
			// Its own failure was recorded already; keep the error text.
			continue
		}
		msg := ""
		if st == store.RestoreItemFailed {
			msg = "could not create the folder"
		}
		out <- store.RestoreOutcome{ItemID: int64(i) + p.base.MinID, Status: st, Flags: int(flags[i]), Err: msg}
	}
}

// walk visits the backup depth-first in batches of directories, creating
// directories that are restored themselves (onDir) and queueing the files
// without a recorded outcome (recorded is nil in a first run).
func (m *Manager) walk(ctx context.Context, j *job, p *plan, tasks chan<- fileTask, recorded []bool,
	ensureDir func(string) error, onDir func(int, dirNode)) error {
	roots, err := m.store.RestoreSourceRoots(ctx, j.backupID)
	if err != nil {
		return err
	}
	var stack []dirNode
	visit := func(it store.RestoreSource, dst string) {
		i := p.index(it.ID)
		if i < 0 || !p.base.OK[i] || p.excluded[i] {
			return // unreadable during the backup, or excluded with everything below
		}
		if it.IsDir {
			d := dirNode{id: it.ID, dst: dst}
			if p.restored(i) {
				onDir(i, d)
			}
			// Later copies of a directory may still hold first copies.
			stack = append(stack, d)
			return
		}
		if recorded != nil && recorded[i] {
			ensureDir(filepath.Dir(dst)) // counts its folder, as the first run did
			return
		}
		if p.restored(i) {
			select {
			case tasks <- fileTask{index: i, id: it.ID, dst: dst, hash: it.Hash, modTime: it.ModTime, replace: recorded != nil}:
			case <-ctx.Done():
			}
		}
	}
	for _, r := range roots {
		visit(r, rootTarget(j.target, r.Name))
	}
	for len(stack) > 0 {
		if ctx.Err() != nil {
			return nil
		}
		k := max(0, len(stack)-walkBatch)
		batch := append([]dirNode(nil), stack[k:]...)
		stack = stack[:k]
		byID := make(map[int64]dirNode, len(batch))
		ids := make([]int64, len(batch))
		for i, d := range batch {
			byID[d.id] = d
			ids[i] = d.id
		}
		children, err := m.store.RestoreSourceChildren(ctx, ids)
		if err != nil {
			return err
		}
		for _, c := range children {
			parent := byID[c.Parent]
			visit(c, filepath.Join(parent.dst, c.Name))
		}
	}
	return nil
}

type buffers struct{ data, raw []byte }

// restoreFile writes one file from the content store and checks its hash.
// It returns the outcome (0 when canceled mid-file; the partial file is
// removed).
func (m *Manager) restoreFile(ctx context.Context, j *job, rd *content.Reader, t fileTask, bufs *buffers, ensureDir func(string) error) (int, string) {
	chunks, err := m.store.FileChunks(ctx, t.hash)
	if err != nil {
		return store.RestoreItemFailed, err.Error()
	}
	if err := ensureDir(filepath.Dir(t.dst)); err != nil {
		return store.RestoreItemFailed, shortError(err)
	}
	if t.replace {
		// Written after the last committed outcome, or partly: write it again.
		if info, err := os.Lstat(t.dst); err == nil && info.Mode().IsRegular() {
			os.Remove(t.dst)
		}
	}
	out, err := os.OpenFile(t.dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return store.RestoreItemFailed, shortError(err)
	}
	h := sha256.New()
	h.Write([]byte{0}) // file hash domain prefix, as in the backup
	var written int64
	fail := func(err error) (int, string) { // a partial or broken file is never left behind
		out.Close() // closing twice is harmless
		os.Remove(t.dst)
		j.bytesDone.Add(-written)
		if err == nil {
			return 0, ""
		}
		if errors.Is(err, content.ErrDamaged) {
			return store.RestoreItemFailed, err.Error()
		}
		return store.RestoreItemFailed, shortError(err)
	}
	for _, c := range chunks {
		if ctx.Err() != nil {
			return fail(nil)
		}
		var data []byte
		data, bufs.raw, err = rd.Chunk(c, bufs.data, bufs.raw)
		if err != nil {
			return fail(err)
		}
		if c.Codec != content.CodecRaw {
			bufs.data = data
		}
		h.Write(data)
		if _, err := out.Write(data); err != nil {
			return fail(err)
		}
		written += int64(len(data))
		j.bytesDone.Add(int64(len(data)))
	}
	if err := out.Close(); err != nil {
		return fail(err)
	}
	var sum [32]byte
	h.Sum(sum[:0])
	if string(sum[:]) != string(t.hash) {
		return fail(fmt.Errorf("%w: the restored file does not match the backup", content.ErrDamaged))
	}
	if t.modTime != 0 {
		mt := time.Unix(0, t.modTime)
		os.Chtimes(t.dst, mt, mt)
	}
	return store.RestoreItemOK, ""
}

func shortError(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	return err.Error()
}
