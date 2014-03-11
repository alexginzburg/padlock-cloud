// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package leveldb

import (
	"errors"
	"runtime"

	"github.com/syndtr/goleveldb/leveldb/comparer"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

var (
	errInvalidIkey = errors.New("leveldb: Iterator: invalid internal key")
)

func (db *DB) newRawIterator(slice *util.Range, ro *opt.ReadOptions) iterator.Iterator {
	s := db.s

	em, fm := db.getMems()
	v := s.version()

	ti := v.getIterators(slice, ro)
	n := len(ti) + 2
	i := make([]iterator.Iterator, 0, n)
	i = append(i, em.NewIterator(slice))
	if fm != nil {
		i = append(i, fm.NewIterator(slice))
	}
	i = append(i, ti...)
	mi := iterator.NewMergedIterator(i, s.cmp, true)
	mi.SetReleaser(&versionReleaser{v: v})
	return mi
}

func (db *DB) newIterator(seq uint64, slice *util.Range, ro *opt.ReadOptions) *dbIter {
	var slice_ *util.Range
	if slice != nil {
		slice_ = &util.Range{}
		if slice.Start != nil {
			slice_.Start = newIKey(slice.Start, kMaxSeq, tSeek)
		}
		if slice.Limit != nil {
			slice_.Limit = newIKey(slice.Limit, kMaxSeq, tSeek)
		}
	}
	rawIter := db.newRawIterator(slice_, ro)
	iter := &dbIter{
		cmp:    db.s.cmp.cmp,
		iter:   rawIter,
		seq:    seq,
		strict: db.s.o.GetStrict(opt.StrictIterator) || ro.GetStrict(opt.StrictIterator),
		key:    make([]byte, 0),
		value:  make([]byte, 0),
	}
	runtime.SetFinalizer(iter, (*dbIter).Release)
	return iter
}

type dir int

const (
	dirReleased dir = iota - 1
	dirSOI
	dirEOI
	dirBackward
	dirForward
)

// dbIter represent an interator states over a database session.
type dbIter struct {
	cmp    comparer.BasicComparer
	iter   iterator.Iterator
	seq    uint64
	strict bool

	dir      dir
	key      []byte
	value    []byte
	err      error
	releaser util.Releaser
}

func (i *dbIter) setErr(err error) {
	i.err = err
	i.key = nil
	i.value = nil
}

func (i *dbIter) iterErr() {
	if err := i.iter.Error(); err != nil {
		i.setErr(err)
	}
}

func (i *dbIter) Valid() bool {
	return i.err == nil && i.dir > dirEOI
}

func (i *dbIter) First() bool {
	if i.err != nil {
		return false
	} else if i.dir == dirReleased {
		i.err = ErrIterReleased
		return false
	}

	if i.iter.First() {
		i.dir = dirSOI
		return i.next()
	}
	i.dir = dirEOI
	i.iterErr()
	return false
}

func (i *dbIter) Last() bool {
	if i.err != nil {
		return false
	} else if i.dir == dirReleased {
		i.err = ErrIterReleased
		return false
	}

	if i.iter.Last() {
		return i.prev()
	}
	i.dir = dirSOI
	i.iterErr()
	return false
}

func (i *dbIter) Seek(key []byte) bool {
	if i.err != nil {
		return false
	} else if i.dir == dirReleased {
		i.err = ErrIterReleased
		return false
	}

	ikey := newIKey(key, i.seq, tSeek)
	if i.iter.Seek(ikey) {
		i.dir = dirSOI
		return i.next()
	}
	i.dir = dirEOI
	i.iterErr()
	return false
}

func (i *dbIter) next() bool {
	for {
		ukey, seq, t, ok := parseIkey(i.iter.Key())
		if ok {
			if seq <= i.seq {
				switch t {
				case tDel:
					// Skip deleted key.
					i.key = append(i.key[:0], ukey...)
					i.dir = dirForward
				case tVal:
					if i.dir == dirSOI || i.cmp.Compare(ukey, i.key) > 0 {
						i.key = append(i.key[:0], ukey...)
						i.value = append(i.value[:0], i.iter.Value()...)
						i.dir = dirForward
						return true
					}
				}
			}
		} else if i.strict {
			i.setErr(errInvalidIkey)
			break
		}
		if !i.iter.Next() {
			i.dir = dirEOI
			i.iterErr()
			break
		}
	}
	return false
}

func (i *dbIter) Next() bool {
	if i.dir == dirEOI || i.err != nil {
		return false
	} else if i.dir == dirReleased {
		i.err = ErrIterReleased
		return false
	}

	if !i.iter.Next() || (i.dir == dirBackward && !i.iter.Next()) {
		i.dir = dirEOI
		i.iterErr()
		return false
	}
	return i.next()
}

func (i *dbIter) prev() bool {
	i.dir = dirBackward
	del := true
	if i.iter.Valid() {
		for {
			ukey, seq, t, ok := parseIkey(i.iter.Key())
			if ok {
				if seq <= i.seq {
					if !del && i.cmp.Compare(ukey, i.key) < 0 {
						return true
					}
					del = (t == tDel)
					if !del {
						i.key = append(i.key[:0], ukey...)
						i.value = append(i.value[:0], i.iter.Value()...)
					}
				}
			} else if i.strict {
				i.setErr(errInvalidIkey)
				return false
			}
			if !i.iter.Prev() {
				break
			}
		}
	}
	if del {
		i.dir = dirSOI
		i.iterErr()
		return false
	}
	return true
}

func (i *dbIter) Prev() bool {
	if i.dir == dirSOI || i.err != nil {
		return false
	} else if i.dir == dirReleased {
		i.err = ErrIterReleased
		return false
	}

	switch i.dir {
	case dirEOI:
		return i.Last()
	case dirForward:
		for i.iter.Prev() {
			ukey, _, _, ok := parseIkey(i.iter.Key())
			if ok {
				if i.cmp.Compare(ukey, i.key) < 0 {
					goto cont
				}
			} else if i.strict {
				i.setErr(errInvalidIkey)
				return false
			}
		}
		i.dir = dirSOI
		i.iterErr()
		return false
	}

cont:
	return i.prev()
}

func (i *dbIter) Key() []byte {
	if i.err != nil || i.dir <= dirEOI {
		return nil
	}
	return i.key
}

func (i *dbIter) Value() []byte {
	if i.err != nil || i.dir <= dirEOI {
		return nil
	}
	return i.value
}

func (i *dbIter) Release() {
	if i.dir != dirReleased {
		// Clear the finalizer.
		runtime.SetFinalizer(i, nil)

		if i.releaser != nil {
			i.releaser.Release()
		}

		i.dir = dirReleased
		i.key = nil
		i.value = nil
		i.iter.Release()
		i.iter = nil
	}
}

func (i *dbIter) SetReleaser(releaser util.Releaser) {
	if i.dir != dirReleased {
		i.releaser = releaser
	}
}

func (i *dbIter) Error() error {
	return i.err
}
