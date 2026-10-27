// Package backupjob runs at most one backup at a time: it connects the scanner
// to the database writer and handles pause, resume and stop.
package backupjob

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/stefan-zemljic/data-organizer/internal/content"
	"github.com/stefan-zemljic/data-organizer/internal/scan"
	"github.com/stefan-zemljic/data-organizer/internal/store"
)

// Transient statuses, only seen while a backup is active.
const (
	StatusPausing   = "pausing"
	StatusStopping  = "stopping"
	StatusAnalyzing = "analyzing" // finding duplicates after completion
)

const flushInterval = time.Second

// Errors returned by Manager methods.
var (
	ErrBusy         = errors.New("another backup is active")
	ErrNotActive    = errors.New("backup is not active")
	ErrInvalidState = errors.New("action not possible in the current backup state")
	ErrInvalidRoots = errors.New("invalid backup paths")
	ErrDeleting     = errors.New("a backup is being deleted; try again when that is done")
)

// Manager runs at most one backup at a time and keeps its live state.
type Manager struct {
	store *store.Store
	opts  scan.Options
	guard func() error

	mu       sync.Mutex
	active   *job
	deleting bool // a backup is being deleted (blocks new backups)
}

type job struct {
	id      int64
	roots   []string
	started time.Time
	resume  chan struct{}
	stop    chan struct{}
	done    chan struct{}

	mu            sync.Mutex
	scanner       *scan.Scanner
	status        string
	stopRequested bool
	leaving       bool // the program exits: save and keep the backup resumable
}

func newJob(id int64, roots []string, started time.Time, status string) *job {
	return &job{
		id:      id,
		roots:   roots,
		started: started,
		status:  status,
		resume:  make(chan struct{}, 1),
		stop:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// New returns a Manager that stores backups in st and scans with opts.
func New(st *store.Store, opts scan.Options) *Manager {
	return &Manager{store: st, opts: opts}
}

// SetGuard installs a check that can refuse new backups and deletions (e.g.
// while a restore runs, which competes for disks and database); call before
// serving.
func (m *Manager) SetGuard(guard func() error) { m.guard = guard }

// Store gives read access to stored backups.
func (m *Manager) Store() *store.Store { return m.store }

// Start begins a backup of roots unless another backup is running or paused.
func (m *Manager) Start(ctx context.Context, roots []string) (store.Backup, error) {
	roots, err := scan.NormalizeRoots(roots)
	if err != nil {
		return store.Backup{}, fmt.Errorf("%w: %v", ErrInvalidRoots, err)
	}

	m.mu.Lock()
	if m.active != nil {
		m.mu.Unlock()
		return store.Backup{}, ErrBusy
	}
	if m.deleting {
		m.mu.Unlock()
		return store.Backup{}, ErrDeleting
	}
	if m.guard != nil {
		if err := m.guard(); err != nil {
			m.mu.Unlock()
			return store.Backup{}, err
		}
	}
	j := newJob(0, roots, time.Now(), store.StatusRunning)
	m.active = j // reserve the slot; the setup below runs without the lock
	m.mu.Unlock()

	firstID, err := m.store.NextItemID(ctx)
	var contents *content.Writer
	if err == nil {
		contents, err = m.store.NewContentWriter(ctx)
	}
	if err == nil {
		j.id, err = m.store.CreateBackup(ctx, roots, firstID, j.started)
	}
	if err != nil {
		if contents != nil {
			contents.Close()
		}
		m.mu.Lock()
		m.active = nil
		m.mu.Unlock()
		return store.Backup{}, err
	}
	m.launch(j, contents, false, func(out chan<- scan.Item, opts scan.Options) *scan.Scanner {
		return scan.New(roots, firstID, out, opts)
	})
	return j.snapshot(), nil
}

// Recover picks up the backup that was active when the program last exited
// (stopped or crashed): a running one continues, a paused one waits for
// Resume or Stop. Only what was not stored yet is read again. Call it
// once, before serving.
func (m *Manager) Recover(ctx context.Context) error {
	sc, ok, err := m.store.RecoverBackup(ctx)
	if err != nil || !ok {
		return err
	}
	partial, err := m.store.LoadPartial(ctx, sc.ID)
	var nextID int64
	if err == nil {
		nextID, err = m.store.NextItemID(ctx)
	}
	var contents *content.Writer
	if err == nil {
		contents, err = m.store.NewContentWriter(ctx)
	}
	if err != nil {
		slog.Error("resuming backup", "backup", sc.ID, "err", err)
		return m.store.MarkInterrupted(ctx, sc.ID, "could not resume: "+err.Error())
	}

	paused := sc.Status == store.StatusPaused
	j := newJob(sc.ID, sc.Roots, sc.StartedAt, sc.Status)
	m.mu.Lock()
	m.active = j
	m.mu.Unlock()
	slog.Info("resuming backup", "backup", sc.ID, "status", sc.Status, "unfinished folders", len(partial.Dirs))
	m.launch(j, contents, paused, func(out chan<- scan.Item, opts scan.Options) *scan.Scanner {
		return scan.Resume(sc.Roots, partial, nextID, out, opts)
	})
	return nil
}

// launch connects a new scanner to the writer and runs it (see control).
func (m *Manager) launch(j *job, contents *content.Writer, paused bool, newScanner func(chan<- scan.Item, scan.Options) *scan.Scanner) {
	out := make(chan scan.Item, 4096)
	writerDone := make(chan error, 1)
	opts := m.opts
	opts.Contents = contents
	go m.write(j, contents, out, writerDone) // must run before the scanner is made, which may emit
	sc := newScanner(out, opts)

	j.mu.Lock()
	j.scanner = sc
	if j.stopRequested { // the writer failed during setup
		sc.Stop()
		paused = false
	}
	j.mu.Unlock()

	go m.control(j, out, writerDone, paused)
}

// control runs the backup phases until the backup completes or is stopped, or
// until the program exits (the backup then stays resumable).
func (m *Manager) control(j *job, out chan scan.Item, writerDone <-chan error, paused bool) {
	ctx := context.Background()
	sc := j.scanner
	final := store.StatusCompleted
	errMsg := ""
	for {
		if !paused {
			sc.Start()
			sc.Wait()
			if sc.Done() {
				break
			}
			if err := sc.Err(); err != nil {
				final, errMsg = store.StatusFailed, err.Error()
				break
			}
		}
		paused = false
		j.mu.Lock()
		stopping, leaving, pausing := j.stopRequested, j.leaving, j.status == StatusPausing
		if !stopping && !leaving {
			j.status = store.StatusPaused
		}
		j.mu.Unlock()
		if stopping {
			final = store.StatusStopped
			break
		}
		if leaving {
			if pausing { // paused by the user just before
				m.setStatus(ctx, j, store.StatusPaused)
			}
			final = ""
			break
		}

		m.setStatus(ctx, j, store.StatusPaused)
		select {
		case <-j.resume:
			m.setStatus(ctx, j, store.StatusRunning)
			sc.Resume()
			continue
		case <-j.stop:
			final = store.StatusStopped
			j.mu.Lock()
			if j.leaving && !j.stopRequested {
				final = ""
			}
			j.mu.Unlock()
		}
		break
	}

	close(out)
	if err := <-writerDone; err != nil {
		final, errMsg = store.StatusFailed, err.Error()
	}
	if final == "" {
		// The program exits; the stored items and directory records let
		// the next start continue (see Recover).
		if err := m.store.SaveProgress(ctx, j.id, sc.Progress()); err != nil {
			slog.Error("saving backup progress", "backup", j.id, "err", err)
		}
		m.mu.Lock()
		m.active = nil
		m.mu.Unlock()
		close(j.done)
		return
	}
	if final == store.StatusCompleted {
		// Still holding the active slot, so no other backup writes meanwhile.
		j.mu.Lock()
		j.status = StatusAnalyzing
		j.mu.Unlock()
		if err := m.store.AnalyzeDuplicates(ctx, j.id); err != nil {
			final, errMsg = store.StatusFailed, err.Error()
		}
	}
	if err := m.store.FinishBackup(ctx, j.id, final, sc.Progress(), time.Now(), errMsg); err != nil {
		slog.Error("saving backup result", "backup", j.id, "err", err)
	}

	m.mu.Lock()
	m.active = nil
	m.mu.Unlock()
	close(j.done)
}

// write stores items in batches. On a database error it stops the backup
// and keeps draining so the workers never block.
func (m *Manager) write(j *job, contents *content.Writer, out <-chan scan.Item, done chan<- error) {
	ctx := context.Background()
	w := m.store.NewItemWriter(j.id, contents)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	var werr error
	fail := func(err error) {
		werr = err
		j.mu.Lock()
		j.stopRequested = true
		j.status = StatusStopping
		if j.scanner != nil {
			j.scanner.Stop()
		}
		j.mu.Unlock()
		select { // a paused backup waits for resume or stop
		case j.stop <- struct{}{}:
		default:
		}
	}
	for {
		select {
		case it, ok := <-out:
			if !ok {
				if werr == nil {
					werr = w.Flush()
				}
				if err := contents.Close(); werr == nil {
					werr = err
				}
				done <- werr
				return
			}
			if werr == nil {
				if err := w.Write(ctx, it); err != nil {
					fail(err)
				}
			}
		case <-ticker.C:
			if werr == nil {
				if err := w.Flush(); err != nil {
					fail(err)
				}
			}
		}
	}
}

func (m *Manager) setStatus(ctx context.Context, j *job, status string) {
	if err := m.store.SetStatus(ctx, j.id, status); err != nil {
		slog.Error("saving backup status", "backup", j.id, "err", err)
	}
}

func (m *Manager) activeJob(id int64) (*job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil || m.active.id != id || m.active.scanner == nil {
		return nil, ErrNotActive
	}
	return m.active, nil
}

// Pause makes the backup's workers exit soon; Resume continues it.
func (m *Manager) Pause(id int64) (store.Backup, error) {
	j, err := m.activeJob(id)
	if err != nil {
		return store.Backup{}, err
	}
	j.mu.Lock()
	if j.status != store.StatusRunning {
		j.mu.Unlock()
		return store.Backup{}, ErrInvalidState
	}
	j.status = StatusPausing
	j.scanner.Pause()
	j.mu.Unlock()
	return j.snapshot(), nil
}

// Resume continues the paused active backup id.
func (m *Manager) Resume(id int64) (store.Backup, error) {
	j, err := m.activeJob(id)
	if err != nil {
		return store.Backup{}, err
	}
	j.mu.Lock()
	if j.status != store.StatusPaused {
		j.mu.Unlock()
		return store.Backup{}, ErrInvalidState
	}
	j.status = store.StatusRunning
	j.mu.Unlock()
	j.resume <- struct{}{}
	return j.snapshot(), nil
}

// Stop ends the backup soon; what was hashed so far is kept.
func (m *Manager) Stop(id int64) (store.Backup, error) {
	j, err := m.activeJob(id)
	if err != nil {
		return store.Backup{}, err
	}
	j.mu.Lock()
	if j.status == StatusAnalyzing {
		j.mu.Unlock()
		return store.Backup{}, ErrInvalidState // the backup itself is already complete
	}
	wasPaused := j.status == store.StatusPaused
	j.status = StatusStopping
	j.stopRequested = true
	j.scanner.Stop()
	j.mu.Unlock()
	if wasPaused {
		select {
		case j.stop <- struct{}{}:
		default:
		}
	}
	return j.snapshot(), nil
}

// Busy reports whether a backup is active or being deleted; both write a lot
// to the database, so restores must not start meanwhile.
func (m *Manager) Busy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active != nil || m.deleting
}

// Delete removes a finished backup and everything derived from it (see
// store.DeleteBackup). It is refused while a backup or restore runs: one long
// write transaction would stall their writers.
func (m *Manager) Delete(ctx context.Context, id int64) (store.Deleted, error) {
	m.mu.Lock()
	switch {
	case m.active != nil:
		m.mu.Unlock()
		return store.Deleted{}, fmt.Errorf("%w: delete when it has finished", ErrBusy)
	case m.deleting:
		m.mu.Unlock()
		return store.Deleted{}, ErrDeleting
	}
	if m.guard != nil {
		if err := m.guard(); err != nil {
			m.mu.Unlock()
			return store.Deleted{}, err
		}
	}
	m.deleting = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.deleting = false
		m.mu.Unlock()
	}()
	// Finish even if the client goes away; rolling back would waste the work.
	return m.store.DeleteBackup(context.WithoutCancel(ctx), id)
}

// Current returns the active backup, if any.
func (m *Manager) Current() (store.Backup, bool) {
	m.mu.Lock()
	j := m.active
	m.mu.Unlock()
	if j == nil || j.id == 0 {
		return store.Backup{}, false
	}
	return j.snapshot(), true
}

// List returns all backups, newest first, with live data for the active one.
func (m *Manager) List(ctx context.Context) ([]store.Backup, error) {
	backups, err := m.store.ListBackups(ctx)
	if err != nil {
		return nil, err
	}
	if cur, ok := m.Current(); ok {
		for i := range backups {
			if backups[i].ID == cur.ID {
				backups[i] = cur
			}
		}
	}
	return backups, nil
}

// Get returns backup id, with live progress if it is the active one.
func (m *Manager) Get(ctx context.Context, id int64) (store.Backup, error) {
	if cur, ok := m.Current(); ok && cur.ID == id {
		return cur, nil
	}
	return m.store.GetBackup(ctx, id)
}

// Shutdown pauses the active backup and waits until everything found so far
// is saved; the next start resumes it (see Recover). A backup finding
// duplicates finishes that first, unless ctx ends (it is then redone).
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	j := m.active
	m.mu.Unlock()
	if j == nil {
		return nil
	}
	j.mu.Lock()
	j.leaving = true
	if j.scanner != nil {
		j.scanner.Pause()
	}
	j.mu.Unlock()
	select { // wakes a paused backup
	case j.stop <- struct{}{}:
	default:
	}
	select {
	case <-j.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (j *job) snapshot() store.Backup {
	j.mu.Lock()
	defer j.mu.Unlock()
	sc := store.Backup{
		ID:        j.id,
		Status:    j.status,
		StartedAt: j.started,
		Roots:     j.roots,
	}
	if j.scanner != nil {
		sc.Progress = j.scanner.Progress()
	}
	return sc
}
