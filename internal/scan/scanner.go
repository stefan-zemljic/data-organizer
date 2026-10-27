// Package scan walks directory trees in parallel and hashes every readable
// file and directory (see hash.go for the format).
package scan

import (
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
)

const readBufferSize = 1 << 20

// Options configure a Scanner.
type Options struct {
	// Workers is the number of goroutines that list directories and hash
	// files. Zero picks a default based on the CPU count.
	Workers int
	// Exclude lists absolute paths to skip, such as the scan database itself.
	Exclude []string
	// Contents, when set, stores every file's content while it is read; it
	// also computes the file hash.
	Contents Contents
}

// Contents stores file contents (content.Writer). StoreFile reads r to the
// end and returns the file hash (see hash.go) and size; it calls progress
// with the bytes read and gives up with stopped set when stop returns true.
type Contents interface {
	StoreFile(r io.Reader, stop func() bool, progress func(n int)) (hash [32]byte, size int64, stopped bool, err error)
}

type dirNode struct {
	id     int64
	parent *dirNode
	path   string
	name   string // name within the parent; the full path for roots

	mu      sync.Mutex
	acc     [32]byte // XOR of the entry digests of finished children
	pending int      // unfinished children, plus one until listed
	mtime   int64

	// Set for unfinished directories of a resumed scan: they are listed
	// again, skipping children stored before and these known unfinished
	// subdirectories (queued on their own).
	resumed bool
	known   []string
}

type fileJob struct {
	id     int64
	parent *dirNode
	name   string // the full path for root files
}

// path is the file's full path, built when needed (keeps the queue small:
// a full C:\ drive has millions of queued files).
func (j fileJob) path() string {
	if j.parent == nil {
		return j.name
	}
	return filepath.Join(j.parent.path, j.name)
}

type excludedPath struct{ base, path string }

// Scanner hashes the trees below a set of roots. Items are sent to out as
// they complete, from many goroutines; the receiver must keep draining it.
//
// A scan first discovers everything (lists all directories, collecting
// files and their sizes), so the total is known, and then hashes the
// files. Start launches the workers (plus extra listers while
// discovering), which exit when all work is done or when Pause or Stop is
// called (a file being hashed is put back into the queue). Wait blocks
// until they have exited; after a pause, Resume and Start continue.
type Scanner struct {
	out      chan<- Item
	workers  int
	listers  int // extra goroutines that only list directories
	exclude  []excludedPath
	contents Contents
	children func(dirID int64) ([]StoredChild, error) // resumed scans only
	nextID   atomic.Int64

	errMu sync.Mutex
	err   error // reading the stored state failed; the scan is stopped

	paused  atomic.Bool
	stopped atomic.Bool
	wg      sync.WaitGroup

	mu         sync.Mutex
	cond       *sync.Cond
	dirQ       []*dirNode
	fileQ      []fileJob
	listing    int  // directories being listed right now
	discovered bool // every directory has been listed
	rootsLeft  int
	done       bool

	dirsFound, dirsDone, filesFound, filesDone, bytesFound, bytesDone, errorCount atomic.Int64
}

// maxListers bounds the goroutines listing directories while discovering:
// listing waits on the file system much more than on the CPU.
const maxListers = 64

// New prepares a scan of roots (see NormalizeRoots), numbering items from
// firstID. Roots that cannot be read are reported on out immediately, so
// the receiver must already be running.
func New(roots []string, firstID int64, out chan<- Item, opts Options) *Scanner {
	s := newScanner(firstID, out, opts)
	s.rootsLeft = len(roots)
	for _, root := range roots {
		s.addRoot(root)
	}
	s.discovered = len(s.dirQ) == 0
	s.done = s.rootsLeft == 0
	return s
}

func newScanner(firstID int64, out chan<- Item, opts Options) *Scanner {
	workers := opts.Workers
	if workers <= 0 {
		workers = min(max(2*runtime.NumCPU(), 4), 32)
	}
	s := &Scanner{out: out, workers: workers, listers: max(0, maxListers-workers), contents: opts.Contents}
	s.cond = sync.NewCond(&s.mu)
	s.nextID.Store(firstID)
	for _, p := range opts.Exclude {
		s.exclude = append(s.exclude, excludedPath{base: filepath.Base(p), path: filepath.Clean(p)})
	}
	return s
}

// addRoot queues a root (counted in rootsLeft by the caller).
func (s *Scanner) addRoot(root string) {
	id := s.newID()
	info, err := os.Lstat(root)
	switch {
	case err != nil:
		s.fail(id, nil, root, KindFile, err)
	case info.IsDir():
		s.dirsFound.Add(1)
		s.out <- Item{ID: id, Name: root, Kind: KindDir, Listed: true}
		s.dirQ = append(s.dirQ, &dirNode{id: id, path: root, name: root, pending: 1, mtime: info.ModTime().UnixNano()})
	case info.Mode().IsRegular():
		s.filesFound.Add(1)
		s.bytesFound.Add(info.Size())
		s.fileQ = append(s.fileQ, fileJob{id: id, name: root})
	default:
		s.fail(id, nil, root, KindFile, errors.New("not a regular file or directory"))
	}
}

// Start launches the workers for one phase, and extra listers while
// directories remain to be listed.
func (s *Scanner) Start() {
	s.mu.Lock()
	listers := 0
	if !s.discovered {
		listers = s.listers
	}
	s.mu.Unlock()
	s.wg.Add(s.workers + listers)
	for range s.workers {
		go s.worker(false)
	}
	for range listers {
		go s.worker(true)
	}
}

// Wait blocks until the workers of the current phase have exited.
func (s *Scanner) Wait() { s.wg.Wait() }

// Pause makes the workers exit soon; the remaining work stays queued.
func (s *Scanner) Pause() { s.interrupt(&s.paused) }

// Resume clears a pause; call Start to continue.
func (s *Scanner) Resume() { s.paused.Store(false) }

// Stop makes the workers exit soon; the scan cannot be continued.
func (s *Scanner) Stop() { s.interrupt(&s.stopped) }

func (s *Scanner) interrupt(flag *atomic.Bool) {
	flag.Store(true)
	s.mu.Lock()
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *Scanner) interrupted() bool { return s.paused.Load() || s.stopped.Load() }

// Err reports why a resumed scan stopped by itself: its stored state
// could not be read. The scan cannot be continued then.
func (s *Scanner) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

func (s *Scanner) abort(err error) {
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
	s.Stop()
}

// Done reports whether every root has been fully processed.
func (s *Scanner) Done() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// Progress returns the current counters.
func (s *Scanner) Progress() Progress {
	s.mu.Lock()
	discovering := !s.discovered
	s.mu.Unlock()
	return Progress{
		Discovering: discovering,
		DirsFound:   s.dirsFound.Load(),
		DirsDone:    s.dirsDone.Load(),
		FilesFound:  s.filesFound.Load(),
		FilesDone:   s.filesDone.Load(),
		BytesFound:  s.bytesFound.Load(),
		BytesDone:   s.bytesDone.Load(),
		Errors:      s.errorCount.Load(),
	}
}

func (s *Scanner) newID() int64 { return s.nextID.Add(1) - 1 }

// worker lists directories until discovery is complete, then (unless it
// is a lister, which exits) hashes files.
func (s *Scanner) worker(lister bool) {
	defer s.wg.Done()
	var buf []byte
	for {
		var dir *dirNode
		var file fileJob
		s.mu.Lock()
		for {
			if s.done || s.interrupted() || (lister && s.discovered) {
				s.mu.Unlock()
				return
			}
			if !s.discovered {
				if n := len(s.dirQ); n > 0 {
					dir, s.dirQ = s.dirQ[n-1], s.dirQ[:n-1]
					s.listing++
					break
				}
			} else if n := len(s.fileQ); n > 0 {
				file, s.fileQ = s.fileQ[n-1], s.fileQ[:n-1]
				break
			}
			s.cond.Wait()
		}
		s.mu.Unlock()

		if dir != nil {
			s.listDir(dir)
			s.mu.Lock()
			s.listing--
			if s.listing == 0 && len(s.dirQ) == 0 && !s.discovered {
				s.discovered = true // hashing can start
				s.cond.Broadcast()
			}
			s.mu.Unlock()
			continue
		}
		if buf == nil {
			buf = make([]byte, readBufferSize)
		}
		if !s.hashFile(file, buf) {
			return
		}
	}
}

// hashFile returns false when interrupted; the job is then queued again.
func (s *Scanner) hashFile(job fileJob, buf []byte) bool {
	f, err := os.Open(job.path())
	if err != nil {
		s.fail(job.id, job.parent, job.name, KindFile, err)
		return true
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		s.fail(job.id, job.parent, job.name, KindFile, err)
		return true
	}
	if !info.Mode().IsRegular() {
		s.fail(job.id, job.parent, job.name, KindFile, errors.New("not a regular file"))
		return true
	}

	var read int64
	progress := func(n int) {
		read += int64(n)
		s.bytesDone.Add(int64(n))
	}
	var sum [32]byte
	var stopped bool
	if s.contents != nil {
		sum, _, stopped, err = s.contents.StoreFile(f, s.interrupted, progress)
	} else {
		sum, stopped, err = hashOnly(f, buf, s.interrupted, progress)
	}
	if stopped {
		s.bytesDone.Add(-read)
		s.mu.Lock()
		s.fileQ = append(s.fileQ, job)
		s.mu.Unlock()
		return false
	}
	if err != nil {
		s.bytesDone.Add(-read)
		s.fail(job.id, job.parent, job.name, KindFile, err)
		return true
	}
	s.out <- Item{
		ID:       job.id,
		ParentID: parentID(job.parent),
		Name:     job.name,
		Kind:     KindFile,
		Size:     read,
		ModTime:  info.ModTime().UnixNano(),
		Hash:     sum,
	}
	s.filesDone.Add(1)
	s.childDone(job.parent, job.name, sum, true)
	return true
}

// listDir lists one directory: subdirectories are queued for listing,
// files (with their listed size) for hashing. The directory's own time
// comes from its parent's listing, so no extra stat is needed.
//
// A resumed directory keeps what is stored: its stored children count as
// finished, its known unfinished subdirectories are queued already, and
// only the rest of the listing is new work.
func (s *Scanner) listDir(node *dirNode) {
	var skip map[string]bool
	var acc [32]byte
	if node.resumed {
		stored, err := s.children(node.id)
		if err != nil {
			s.abort(err) // the guard stays: the directory never completes
			return
		}
		skip = make(map[string]bool, len(stored)+len(node.known))
		for _, c := range stored {
			skip[c.Name] = true
			if c.OK {
				xorInto(&acc, entryDigest(c.Name, c.Hash))
			}
		}
		for _, name := range node.known {
			skip[name] = true
		}
	}

	f, err := os.Open(node.path)
	if err != nil {
		s.fail(node.id, node.parent, node.name, KindDir, err)
		return
	}
	entries, err := f.ReadDir(-1) // unsorted, unlike os.ReadDir
	f.Close()
	if err != nil {
		s.fail(node.id, node.parent, node.name, KindDir, err)
		return
	}

	var dirs []*dirNode
	var files []fileJob
	var bytes int64
	for _, e := range entries {
		t := e.Type()
		if !t.IsDir() && !t.IsRegular() {
			continue // symlinks, junctions, devices, ...
		}
		name := e.Name()
		if skip[name] || s.excluded(node.path, name) {
			continue
		}
		info, err := e.Info() // free on Windows: part of the listing
		if err != nil {
			continue // vanished since listing
		}
		if t.IsDir() {
			dirs = append(dirs, &dirNode{id: s.newID(), parent: node, path: filepath.Join(node.path, name), name: name, pending: 1,
				mtime: info.ModTime().UnixNano()})
		} else {
			files = append(files, fileJob{id: s.newID(), parent: node, name: name})
			bytes += info.Size()
		}
	}
	// The new children are not visible to other workers yet; known
	// subdirectories of a resumed directory may be finishing meanwhile.
	node.mu.Lock()
	node.pending += len(dirs) + len(files)
	xorInto(&node.acc, acc)
	node.mu.Unlock()
	s.dirsFound.Add(int64(len(dirs)))
	s.filesFound.Add(int64(len(files)))
	s.bytesFound.Add(bytes)
	for _, d := range dirs {
		s.out <- Item{ID: d.id, ParentID: node.id, Name: d.name, Kind: KindDir, Listed: true}
	}

	s.mu.Lock()
	s.dirQ = append(s.dirQ, dirs...)
	s.fileQ = append(s.fileQ, files...)
	s.cond.Broadcast()
	s.mu.Unlock()

	s.release(node, nil) // drop the listing guard
}

// fail records an unreadable item. It is left out of its parent's hash.
func (s *Scanner) fail(id int64, parent *dirNode, name string, kind Kind, err error) {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = errors.New(pe.Op + ": " + pe.Err.Error()) // the path is implied by the item
	}
	s.out <- Item{ID: id, ParentID: parentID(parent), Name: name, Kind: kind, Err: err.Error()}
	s.errorCount.Add(1)
	s.childDone(parent, name, [32]byte{}, false)
}

// childDone reports a finished child to its parent; ok=false leaves the
// child out of the parent's hash.
func (s *Scanner) childDone(parent *dirNode, name string, hash [32]byte, ok bool) {
	if parent == nil {
		s.rootDone()
		return
	}
	if ok {
		d := entryDigest(name, hash)
		s.release(parent, &d)
	} else {
		s.release(parent, nil)
	}
}

// release folds an optional entry digest into node and decrements its
// pending count; the last release completes the directory.
func (s *Scanner) release(node *dirNode, digest *[32]byte) {
	node.mu.Lock()
	if digest != nil {
		xorInto(&node.acc, *digest)
	}
	node.pending--
	last := node.pending == 0
	node.mu.Unlock()
	if !last {
		return
	}

	hash := dirHash(node.acc)
	s.out <- Item{
		ID:       node.id,
		ParentID: parentID(node.parent),
		Name:     node.name,
		Kind:     KindDir,
		ModTime:  node.mtime,
		Hash:     hash,
	}
	s.dirsDone.Add(1)
	s.childDone(node.parent, node.name, hash, true)
}

func (s *Scanner) rootDone() {
	s.mu.Lock()
	s.rootsLeft--
	if s.rootsLeft == 0 {
		s.done = true
		s.cond.Broadcast()
	}
	s.mu.Unlock()
}

// excluded reports whether name in dir is an excluded path; the full path
// is only built when the name matches.
func (s *Scanner) excluded(dir, name string) bool {
	for _, ex := range s.exclude {
		if strings.EqualFold(name, ex.base) && strings.EqualFold(filepath.Join(dir, name), ex.path) {
			return true
		}
	}
	return false
}

func parentID(parent *dirNode) int64 {
	if parent == nil {
		return 0
	}
	return parent.id
}

// hashOnly hashes a file without storing it (scans without Contents).
func hashOnly(f io.Reader, buf []byte, stop func() bool, progress func(int)) (sum [32]byte, stopped bool, err error) {
	h := sha256.New()
	h.Write([]byte{filePrefix})
	for {
		if stop() {
			return sum, true, nil
		}
		n, err := f.Read(buf)
		h.Write(buf[:n])
		progress(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			return sum, false, err
		}
	}
	h.Sum(sum[:0])
	return sum, false, nil
}
