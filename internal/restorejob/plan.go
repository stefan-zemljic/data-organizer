package restorejob

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"

	"github.com/stefan-zemljic/data-organizer/internal/store"
)

// plan decides, per item of a backup, whether it is restored: it must have
// been readable, not be excluded (itself or an ancestor marked) and be the
// first copy by path among the copies that are not excluded.
type plan struct {
	base     *store.PlanBase
	excluded []bool
	later    []bool // a duplicate with an earlier included copy
}

func makePlan(ctx context.Context, st *store.Store, backupID int64) (*plan, error) {
	base, err := st.RestorePlanBase(ctx, backupID)
	if err != nil {
		return nil, err
	}
	p := &plan{base: base, excluded: make([]bool, len(base.Parent)), later: make([]bool, len(base.Parent))}
	// Ids grow from parents to children, so one pass in id order resolves
	// exclusion through ancestors.
	for i := range base.Parent {
		ex := base.Marked[i]
		if !ex && base.Parent[i] != 0 {
			if pi := p.index(base.Parent[i]); pi >= 0 {
				ex = p.excluded[pi]
			}
		}
		p.excluded[i] = ex
	}
	var group []byte
	taken := false
	err = st.DuplicatesInRankOrder(ctx, backupID, func(id int64, hash []byte) {
		if !bytes.Equal(hash, group) {
			group, taken = hash, false
		}
		i := p.index(id)
		if i < 0 {
			return
		}
		if !p.excluded[i] && !taken {
			taken = true // the first included copy is the one restored
			return
		}
		p.later[i] = true
	})
	return p, err
}

func (p *plan) index(id int64) int {
	i := int(id - p.base.MinID)
	if i < 0 || i >= len(p.base.Parent) || !p.base.Exists[i] {
		return -1
	}
	return i
}

// restored reports whether the item itself is restored (directories: created).
func (p *plan) restored(i int) bool {
	return p.base.OK[i] && !p.excluded[i] && !p.later[i]
}

// totals counts the files to restore and their size.
func (p *plan) totals() (files, bytes int64) {
	for i := range p.base.Parent {
		if p.base.Exists[i] && !p.base.IsDir[i] && p.restored(i) {
			files++
			bytes += p.base.Size[i]
		}
	}
	return files, bytes
}

// rootTarget maps a backed-up root path into the target folder, keeping the
// drive or share as a folder: C:\Users\x -> <target>\C\Users\x.
func rootTarget(target, root string) string {
	vol := filepath.VolumeName(root)
	rest := strings.TrimLeft(root[len(vol):], `\/`)
	v := strings.Trim(strings.NewReplacer(":", "", `\`, "_", "/", "_").Replace(vol), "_")
	if v == "" {
		return filepath.Join(target, rest)
	}
	return filepath.Join(target, v, rest)
}
