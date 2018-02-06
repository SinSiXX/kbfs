// Copyright 2016-2017 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package simplefs

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	stdpath "path"
	"strings"
	"sync"

	"golang.org/x/net/context"

	"github.com/keybase/client/go/logger"
	"github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/kbfs/kbfscrypto"
	"github.com/keybase/kbfs/libfs"
	"github.com/keybase/kbfs/libkbfs"
	"github.com/keybase/kbfs/tlf"
	billy "gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-billy.v4/osfs"
)

const (
	// CtxOpID is the display name for the unique operation SimpleFS ID tag.
	ctxOpID = "SFSID"
)

// CtxTagKey is the type used for unique context tags
type ctxTagKey int

const (
	// CtxIDKey is the type of the tag for unique operation IDs.
	ctxIDKey ctxTagKey = iota
)

// SimpleFS is the simple filesystem rpc layer implementation.
type SimpleFS struct {
	// log for logging - constant, does not need locking.
	log logger.Logger
	// config for the fs - constant, does not need locking.
	config libkbfs.Config
	// lock protects handles and inProgress
	lock sync.RWMutex
	// handles contains handles opened by SimpleFSOpen,
	// closed by SimpleFSClose (or SimpleFSCancel) and used
	// by listing, reading and writing.
	handles map[keybase1.OpID]*handle
	// inProgress is for keeping state of operations in progress,
	// values are removed by SimpleFSWait (or SimpleFSCancel).
	inProgress map[keybase1.OpID]*inprogress
}

type inprogress struct {
	desc   keybase1.OpDescription
	cancel context.CancelFunc
	done   chan error
}

type handle struct {
	node  libkbfs.Node
	async interface{}
	path  keybase1.Path
}

// make sure the interface is implemented
var _ keybase1.SimpleFSInterface = (*SimpleFS)(nil)

// NewSimpleFS creates a new SimpleFS instance.
func NewSimpleFS(config libkbfs.Config) keybase1.SimpleFSInterface {
	return newSimpleFS(config)
}

func newSimpleFS(config libkbfs.Config) *SimpleFS {
	log := config.MakeLogger("simplefs")
	return &SimpleFS{
		config:     config,
		handles:    map[keybase1.OpID]*handle{},
		inProgress: map[keybase1.OpID]*inprogress{},
		log:        log,
	}
}

func (k *SimpleFS) makeContext(ctx context.Context) context.Context {
	return libkbfs.CtxWithRandomIDReplayable(ctx, ctxIDKey, ctxOpID, k.log)
}

// remotePath decodes a remote path for us.
func remotePath(path keybase1.Path) (
	t tlf.Type, tlfName, middlePath, finalElem string, err error) {
	pt, err := path.PathType()
	if err != nil {
		return tlf.Private, "", "", "", err
	}
	if pt != keybase1.PathType_KBFS {
		return tlf.Private, "", "", "", errOnlyRemotePathSupported
	}
	raw := path.Kbfs()
	if raw != `` && raw[0] == '/' {
		raw = raw[1:]
	}
	ps := strings.Split(raw, `/`)
	switch {
	case len(ps) < 2:
		return tlf.Private, "", "", "", errInvalidRemotePath
	case ps[0] == `private`:
		t = tlf.Private
	case ps[0] == `public`:
		t = tlf.Public
	case ps[0] == `team`:
		t = tlf.SingleTeam
	default:
		return tlf.Private, "", "", "", errInvalidRemotePath

	}
	finalElem = ps[len(ps)-1]
	return t, ps[1], stdpath.Join(ps[2 : len(ps)-1]...), finalElem, nil
}

func (k *SimpleFS) getFS(ctx context.Context, path keybase1.Path) (
	fs billy.Filesystem, finalElem string, err error) {
	pt, err := path.PathType()
	if err != nil {
		return nil, "", err
	}
	switch pt {
	case keybase1.PathType_KBFS:
		t, tlfName, restOfPath, finalElem, err := remotePath(path)
		if err != nil {
			return nil, "", err
		}
		tlfHandle, err := libkbfs.GetHandleFromFolderNameAndType(
			ctx, k.config.KBPKI(), k.config.MDOps(), tlfName, t)
		if err != nil {
			return nil, "", err
		}
		fs, err := libfs.NewFS(
			ctx, k.config, tlfHandle, restOfPath, "", keybase1.MDPriorityNormal)
		if err != nil {
			return nil, "", err
		}
		return fs, finalElem, nil
	case keybase1.PathType_LOCAL:
		fs = osfs.New(stdpath.Dir(path.Local()))
		return fs, stdpath.Base(path.Local()), nil
	default:
		return nil, "", simpleFSError{"Invalid path type"}
	}
}

func (k *SimpleFS) favoriteList(ctx context.Context, path keybase1.Path, t tlf.Type) ([]keybase1.Dirent, error) {
	session, err := k.config.KBPKI().GetCurrentSession(ctx)
	// Return empty directory listing if we are not logged in.
	if err != nil {
		return nil, nil
	}

	favs, err := k.config.KBFSOps().GetFavorites(ctx)
	if err != nil {
		return nil, err
	}

	res := make([]keybase1.Dirent, len(favs))
	for i, fav := range favs {
		if fav.Type != t {
			continue
		}
		pname, err := tlf.CanonicalToPreferredName(
			session.Name, tlf.CanonicalName(fav.Name))
		if err != nil {
			k.log.Errorf("CanonicalToPreferredName: %q %v", fav.Name, err)
			continue
		}
		res[i].Name = string(pname)
		res[i].DirentType = deTy2Ty(libkbfs.Dir)
	}
	return res, nil
}

// SimpleFSList - Begin list of items in directory at path
// Retrieve results with readList()
// Cannot be a single file to get flags/status,
// must be a directory.
func (k *SimpleFS) SimpleFSList(ctx context.Context, arg keybase1.SimpleFSListArg) error {
	return k.startAsync(ctx, arg.OpID, keybase1.NewOpDescriptionWithList(
		keybase1.ListArgs{
			OpID: arg.OpID, Path: arg.Path,
		}), func(ctx context.Context) (err error) {
		var res []keybase1.Dirent

		rawPath := arg.Path.Kbfs()
		switch {
		case rawPath == `/public`:
			res, err = k.favoriteList(ctx, arg.Path, tlf.Public)
		case rawPath == `/private`:
			res, err = k.favoriteList(ctx, arg.Path, tlf.Private)
		case rawPath == `/team`:
			res, err = k.favoriteList(ctx, arg.Path, tlf.SingleTeam)
		default:
			fs, finalElem, err := k.getFS(ctx, arg.Path)
			if err != nil {
				return err
			}

			fi, err := fs.Stat(finalElem)
			if err != nil {
				return err
			}
			var fis []os.FileInfo
			if fi.IsDir() {
				fis, err = fs.ReadDir(finalElem)
			} else {
				fis = append(fis, fi)
			}
			for _, fi := range fis {
				var d keybase1.Dirent
				setStat(&d, fi)
				d.Name = fi.Name()
				res = append(res, d)
			}
		}
		k.setResult(arg.OpID, keybase1.SimpleFSListResult{Entries: res})
		return nil
	})
}

// SimpleFSListRecursive - Begin recursive list of items in directory at path
func (k *SimpleFS) SimpleFSListRecursive(ctx context.Context, arg keybase1.SimpleFSListRecursiveArg) error {
	return k.startAsync(ctx, arg.OpID, keybase1.NewOpDescriptionWithListRecursive(
		keybase1.ListArgs{
			OpID: arg.OpID, Path: arg.Path,
		}), func(ctx context.Context) (err error) {

		// A stack of paths to process - ordering does not matter.
		// Here we don't walk symlinks, so no loops possible.
		var paths []string

		fs, finalElem, err := k.getFS(ctx, arg.Path)
		if err != nil {
			return err
		}
		fi, err := fs.Stat(finalElem)
		if err != nil {
			return err
		}
		var des []keybase1.Dirent
		if !fi.IsDir() {
			var d keybase1.Dirent
			setStat(&d, fi)
			d.Name = fi.Name()
			des = append(des, d)
			// Leave paths empty so we can skip the loop below.
		} else {
			paths = append(paths, finalElem)
		}

		for len(paths) > 0 {
			// Take last element and shorten.
			path := paths[len(paths)-1]
			paths = paths[:len(paths)-1]

			fis, err := fs.ReadDir(path)
			if err != nil {
				return err
			}
			for _, fi := range fis {
				var de keybase1.Dirent
				setStat(&de, fi)
				name := stdpath.Join(path, fi.Name())
				de.Name = name
				des = append(des, de)
				if fi.IsDir() {
					paths = append(paths, name)
				}
			}
		}
		k.setResult(arg.OpID, keybase1.SimpleFSListResult{Entries: des})

		return nil
	})
}

// SimpleFSReadList - Get list of Paths in progress. Can indicate status of pending
// to get more entries.
func (k *SimpleFS) SimpleFSReadList(_ context.Context, opid keybase1.OpID) (keybase1.SimpleFSListResult, error) {
	k.lock.Lock()
	res, _ := k.handles[opid]
	var x interface{}
	if res != nil {
		x = res.async
		res.async = nil
	}
	k.lock.Unlock()

	lr, ok := x.(keybase1.SimpleFSListResult)
	if !ok {
		return keybase1.SimpleFSListResult{}, errNoResult
	}

	return lr, nil
}

// SimpleFSCopy - Begin copy of file or directory
func (k *SimpleFS) SimpleFSCopy(ctx context.Context, arg keybase1.SimpleFSCopyArg) error {
	return k.startAsync(ctx, arg.OpID, keybase1.NewOpDescriptionWithCopy(
		keybase1.CopyArgs{OpID: arg.OpID, Src: arg.Src, Dest: arg.Dest}),
		func(ctx context.Context) (err error) {
			return k.doCopy(ctx, arg.Src, arg.Dest)
		})
}

func (k *SimpleFS) doCopy(ctx context.Context, srcPath, destPath keybase1.Path) error {
	// Note this is also used by move, so if this changes update SimpleFSMove
	// code also.
	src, err := k.pathIO(ctx, srcPath, keybase1.OpenFlags_READ|keybase1.OpenFlags_EXISTING, nil)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := k.pathIO(ctx, destPath, keybase1.OpenFlags_WRITE|keybase1.OpenFlags_REPLACE, src)
	if err != nil {
		return err
	}
	defer dst.Close()

	if src.Type() == keybase1.DirentType_FILE || src.Type() == keybase1.DirentType_EXEC {
		err = copyWithCancellation(ctx, dst, src)
		if err != nil {
			return err
		}
	}

	return nil
}

func copyWithCancellation(ctx context.Context, dst io.Writer, src io.Reader) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := io.CopyN(dst, src, 64*1024)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type pathPair struct {
	src, dest keybase1.Path
}

// SimpleFSCopyRecursive - Begin recursive copy of directory
func (k *SimpleFS) SimpleFSCopyRecursive(ctx context.Context,
	arg keybase1.SimpleFSCopyRecursiveArg) error {
	return k.startAsync(ctx, arg.OpID, keybase1.NewOpDescriptionWithCopy(
		keybase1.CopyArgs{OpID: arg.OpID, Src: arg.Src, Dest: arg.Dest}),
		func(ctx context.Context) (err error) {

			var paths = []pathPair{{src: arg.Src, dest: arg.Dest}}
			for len(paths) > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				// wrap in a function for defers.
				err = func() error {
					path := paths[len(paths)-1]
					paths = paths[:len(paths)-1]

					src, err := k.pathIO(ctx, path.src, keybase1.OpenFlags_READ|keybase1.OpenFlags_EXISTING, nil)
					if err != nil {
						return err
					}
					defer src.Close()

					dst, err := k.pathIO(ctx, path.dest, keybase1.OpenFlags_WRITE|keybase1.OpenFlags_REPLACE, src)
					if err != nil {
						return err
					}
					defer dst.Close()

					// TODO symlinks
					switch src.Type() {
					case keybase1.DirentType_FILE, keybase1.DirentType_EXEC:
						err = copyWithCancellation(ctx, dst, src)
						if err != nil {
							return err
						}
					case keybase1.DirentType_DIR:
						eis, err := src.Children()
						if err != nil {
							return err
						}
						for name := range eis {
							paths = append(paths, pathPair{
								src:  pathAppend(path.src, name),
								dest: pathAppend(path.dest, name),
							})
						}
					}
					return nil
				}()
			}

			return err
		})
}

func pathAppend(p keybase1.Path, leaf string) keybase1.Path {
	if p.Local__ != nil {
		var s = *p.Local__ + "/" + leaf
		p.Local__ = &s
	} else if p.Kbfs__ != nil {
		var s = *p.Kbfs__ + "/" + leaf
		p.Kbfs__ = &s
	}
	return p
}

// SimpleFSMove - Begin move of file or directory, from/to KBFS only
func (k *SimpleFS) SimpleFSMove(ctx context.Context, arg keybase1.SimpleFSMoveArg) error {
	return k.startAsync(ctx, arg.OpID, keybase1.NewOpDescriptionWithMove(
		keybase1.MoveArgs{
			OpID: arg.OpID, Src: arg.Src, Dest: arg.Dest,
		}), func(ctx context.Context) (err error) {

		err = k.doCopy(ctx, arg.Src, arg.Dest)
		if err != nil {
			return err
		}
		pt, err := arg.Src.PathType()
		if err != nil {
			// should really not happen...
			return err
		}
		switch pt {
		case keybase1.PathType_KBFS:
			err = k.doRemove(ctx, arg.Src)
		case keybase1.PathType_LOCAL:
			err = os.Remove(arg.Src.Local())
		}
		return err
	})
}

// SimpleFSRename - Rename file or directory, KBFS side only
func (k *SimpleFS) SimpleFSRename(ctx context.Context, arg keybase1.SimpleFSRenameArg) (err error) {
	// This is not async.
	ctx, err = k.startSyncOp(ctx, "Rename", arg)
	if err != nil {
		return err
	}
	defer func() { k.doneSyncOp(ctx, err) }()

	// Get root FS, to be shared by both src and dst.
	t, tlfName, restOfSrcPath, finalSrcElem, err := remotePath(arg.Src)
	if err != nil {
		return err
	}
	tlfHandle, err := libkbfs.GetHandleFromFolderNameAndType(
		ctx, k.config.KBPKI(), k.config.MDOps(), tlfName, t)
	if err != nil {
		return err
	}
	fs, err := libfs.NewFS(
		ctx, k.config, tlfHandle, "", "", keybase1.MDPriorityNormal)
	if err != nil {
		return err
	}

	// Make sure src and dst share the same TLF.
	tDst, tlfNameDst, restOfDstPath, finalDstElem, err := remotePath(arg.Dest)
	if err != nil {
		return err
	}
	if tDst != t || tlfName != tlfNameDst {
		return simpleFSError{"Cannot rename across top-level folders"}
	}

	err = fs.Rename(
		fs.Join(restOfSrcPath, finalSrcElem),
		fs.Join(restOfDstPath, finalDstElem))
	return err
}

// SimpleFSOpen - Create/open a file and leave it open
// or create a directory
// Files must be closed afterwards.
func (k *SimpleFS) SimpleFSOpen(ctx context.Context, arg keybase1.SimpleFSOpenArg) (err error) {
	ctx, err = k.startSyncOp(ctx, "Open", arg)
	if err != nil {
		return err
	}
	defer func() { k.doneSyncOp(ctx, err) }()

	node, _, err := k.open(ctx, arg.Dest, arg.Flags)

	if err != nil {
		return err
	}

	k.lock.Lock()
	k.handles[arg.OpID] = &handle{node: node, path: arg.Dest}
	k.lock.Unlock()

	return nil
}

// SimpleFSSetStat - Set/clear file bits - only executable for now
func (k *SimpleFS) SimpleFSSetStat(ctx context.Context, arg keybase1.SimpleFSSetStatArg) (err error) {
	ctx, err = k.startSyncOp(ctx, "SetStat", arg)
	if err != nil {
		return err
	}
	defer func() { k.doneSyncOp(ctx, err) }()

	fs, finalElem, err := k.getFS(ctx, arg.Dest)
	if err != nil {
		return err
	}
	fi, err := fs.Stat(finalElem)
	if err != nil {
		return err
	}

	mode := fi.Mode()
	const exec = 0100
	switch arg.Flag {
	case keybase1.DirentType_EXEC:
		mode |= 0100
	case keybase1.DirentType_FILE:
		mode &= 0677
	default:
		return nil
	}

	changeFS, ok := fs.(billy.Change)
	if !ok {
		panic(fmt.Sprintf("Unexpected non-Change FS: %T", fs))
	}

	return changeFS.Chmod(finalElem, mode)
}

func (k *SimpleFS) startReadWriteOp(ctx context.Context, opid keybase1.OpID, desc keybase1.OpDescription) (context.Context, error) {
	ctx, err := k.startSyncOp(ctx, desc.AsyncOp__.String(), desc)
	if err != nil {
		return nil, err
	}
	k.lock.RLock()
	k.inProgress[opid] = &inprogress{desc, func() {}, make(chan error, 1)}
	k.lock.RUnlock()
	return ctx, err
}

func (k *SimpleFS) doneReadWriteOp(ctx context.Context, opID keybase1.OpID, err error) {
	k.lock.RLock()
	delete(k.inProgress, opID)
	k.lock.RUnlock()
	k.log.CDebugf(ctx, "doneReadWriteOp, status=%v", err)
	if ctx != nil {
		libkbfs.CleanupCancellationDelayer(ctx)
	}
}

// SimpleFSRead - Read (possibly partial) contents of open file,
// up to the amount specified by size.
// Repeat until zero bytes are returned or error.
// If size is zero, read an arbitrary amount.
func (k *SimpleFS) SimpleFSRead(ctx context.Context,
	arg keybase1.SimpleFSReadArg) (_ keybase1.FileContent, err error) {
	ctx = k.makeContext(ctx)
	k.lock.RLock()
	h, ok := k.handles[arg.OpID]
	k.lock.RUnlock()
	if !ok {
		return keybase1.FileContent{}, errNoSuchHandle
	}
	opDesc := keybase1.NewOpDescriptionWithRead(
		keybase1.ReadArgs{
			OpID:   arg.OpID,
			Path:   h.path,
			Offset: arg.Offset,
			Size:   arg.Size,
		})
	ctx, err = k.startReadWriteOp(ctx, arg.OpID, opDesc)
	if err != nil {
		return keybase1.FileContent{}, err
	}

	defer func() { k.doneReadWriteOp(ctx, arg.OpID, err) }()

	bs := make([]byte, arg.Size)
	n, err := k.config.KBFSOps().Read(ctx, h.node, bs, arg.Offset)
	bs = bs[:n]
	return keybase1.FileContent{
		Data: bs,
	}, err
}

// SimpleFSWrite - Append content to opened file.
// May be repeated until OpID is closed.
func (k *SimpleFS) SimpleFSWrite(ctx context.Context, arg keybase1.SimpleFSWriteArg) error {
	ctx = k.makeContext(ctx)
	k.lock.RLock()
	h, ok := k.handles[arg.OpID]
	k.lock.RUnlock()
	if !ok {
		return errNoSuchHandle
	}

	opDesc := keybase1.NewOpDescriptionWithWrite(
		keybase1.WriteArgs{
			OpID: arg.OpID, Path: h.path, Offset: arg.Offset,
		})

	ctx, err := k.startReadWriteOp(ctx, arg.OpID, opDesc)
	if err != nil {
		return err
	}
	defer func() { k.doneReadWriteOp(ctx, arg.OpID, err) }()

	err = k.config.KBFSOps().Write(ctx, h.node, arg.Content, arg.Offset)
	return err
}

// SimpleFSRemove - Remove file or directory from filesystem
func (k *SimpleFS) SimpleFSRemove(ctx context.Context,
	arg keybase1.SimpleFSRemoveArg) error {
	return k.startAsync(ctx, arg.OpID, keybase1.NewOpDescriptionWithRemove(
		keybase1.RemoveArgs{
			OpID: arg.OpID, Path: arg.Path,
		}), func(ctx context.Context) (err error) {
		return k.doRemove(ctx, arg.Path)
	})
}

func (k *SimpleFS) doRemove(ctx context.Context, path keybase1.Path) error {
	node, leaf, err := k.getRemoteNodeParent(ctx, path)
	if err != nil {
		return err
	}
	_, ei, err := k.config.KBFSOps().Lookup(ctx, node, leaf)
	if err != nil {
		return err
	}
	switch ei.Type {
	case libkbfs.Dir:
		err = k.config.KBFSOps().RemoveDir(ctx, node, leaf)
	default:
		err = k.config.KBFSOps().RemoveEntry(ctx, node, leaf)
	}
	return err
}

// SimpleFSStat - Get info about file
func (k *SimpleFS) SimpleFSStat(ctx context.Context, path keybase1.Path) (_ keybase1.Dirent, err error) {
	ctx, err = k.startSyncOp(ctx, "Stat", path)
	if err != nil {
		return keybase1.Dirent{}, err
	}
	defer func() { k.doneSyncOp(ctx, err) }()

	fs, finalElem, err := k.getFS(ctx, path)
	if err != nil {
		return keybase1.Dirent{}, err
	}
	fi, err := fs.Stat(finalElem)
	if err != nil {
		return keybase1.Dirent{}, err
	}

	return wrapStat(fi, err)
}

// SimpleFSMakeOpid - Convenience helper for generating new random value
func (k *SimpleFS) SimpleFSMakeOpid(_ context.Context) (keybase1.OpID, error) {
	var opid keybase1.OpID
	err := kbfscrypto.RandRead(opid[:])
	return opid, err
}

// SimpleFSClose - Close removes a handle associated with Open / List.
func (k *SimpleFS) SimpleFSClose(ctx context.Context, opid keybase1.OpID) (err error) {
	ctx, err = k.startSyncOp(ctx, "Close", opid)
	if err != nil {
		return err
	}
	defer func() { k.doneSyncOp(ctx, err) }()

	k.lock.Lock()
	defer k.lock.Unlock()
	delete(k.inProgress, opid)
	h, ok := k.handles[opid]
	if !ok {
		return errNoSuchHandle
	}
	delete(k.handles, opid)
	if h.node != nil {
		err = k.config.KBFSOps().SyncAll(ctx, h.node.GetFolderBranch())
	}
	return err
}

// SimpleFSCancel starts to cancel op with the given opid.
// Also remove any pending references of opid everywhere.
// Returns before cancellation is guaranteeded to be done - that
// may take some time. Currently always returns nil.
func (k *SimpleFS) SimpleFSCancel(_ context.Context, opid keybase1.OpID) error {
	k.lock.Lock()
	defer k.lock.Unlock()
	delete(k.handles, opid)
	w, ok := k.inProgress[opid]
	if !ok {
		return nil
	}
	delete(k.inProgress, opid)
	w.cancel()
	return nil
}

// SimpleFSCheck - Check progress of pending operation
// Progress variable is still TBD.
// Return errNoResult if no operation found.
func (k *SimpleFS) SimpleFSCheck(_ context.Context, opid keybase1.OpID) (keybase1.Progress, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	if _, ok := k.inProgress[opid]; ok {
		return 0, nil
	} else if _, ok := k.handles[opid]; ok {
		return 0, nil
	}
	return 0, errNoResult
}

// SimpleFSGetOps - Get all the outstanding operations
func (k *SimpleFS) SimpleFSGetOps(_ context.Context) ([]keybase1.OpDescription, error) {
	k.lock.RLock()
	r := make([]keybase1.OpDescription, 0, len(k.inProgress))
	for _, p := range k.inProgress {
		r = append(r, p.desc)
	}
	k.lock.RUnlock()
	return r, nil
}

// SimpleFSWait - Blocking wait for the pending operation to finish
func (k *SimpleFS) SimpleFSWait(ctx context.Context, opid keybase1.OpID) error {
	ctx = k.makeContext(ctx)
	k.lock.RLock()
	w, ok := k.inProgress[opid]
	k.lock.RUnlock()
	k.log.CDebugf(ctx, "Wait %X -> %v, %v", opid, w, ok)
	if !ok {
		return errNoSuchHandle
	}

	err, ok := <-w.done

	k.lock.Lock()
	delete(k.inProgress, opid)
	k.lock.Unlock()

	if !ok {
		return errNoResult
	}
	return err
}

func wrapStat(fi os.FileInfo, err error) (keybase1.Dirent, error) {
	if err != nil {
		return keybase1.Dirent{}, err
	}
	var de keybase1.Dirent
	setStat(&de, fi)
	return de, nil
}

func setStat(de *keybase1.Dirent, fi os.FileInfo) {
	de.Time = keybase1.ToTime(fi.ModTime())
	de.Size = int(fi.Size()) // TODO: FIX protocol
	t := libkbfs.File
	if fi.IsDir() {
		t = libkbfs.Dir
	} else if fi.Mode()&0100 != 0 {
		t = libkbfs.Exec
	} else if fi.Mode()&os.ModeSymlink != 0 {
		t = libkbfs.Sym
	}
	de.DirentType = deTy2Ty(t)
}

func deTy2Ty(et libkbfs.EntryType) keybase1.DirentType {
	switch et {
	case libkbfs.Exec:
		return keybase1.DirentType_EXEC
	case libkbfs.File:
		return keybase1.DirentType_FILE
	case libkbfs.Dir:
		return keybase1.DirentType_DIR
	case libkbfs.Sym:
		return keybase1.DirentType_SYM
	}
	panic("deTy2Ty unreachable")
}

type ioer interface {
	io.ReadWriteCloser
	Type() keybase1.DirentType
	Children() (map[string]libkbfs.EntryInfo, error)
}

func (k *SimpleFS) pathIO(ctx context.Context, path keybase1.Path,
	flags keybase1.OpenFlags, like ioer) (ioer, error) {
	pt, err := path.PathType()
	if err != nil {
		return nil, err
	}
	if like != nil && like.Type() == keybase1.DirentType_DIR {
		flags |= keybase1.OpenFlags_DIRECTORY
	}
	switch pt {
	case keybase1.PathType_KBFS:
		node, ei, err := k.open(ctx, path, flags)
		if err != nil {
			return nil, err
		}
		return &kbfsIO{ctx, k, node, 0, deTy2Ty(ei.Type)}, nil
	case keybase1.PathType_LOCAL:
		var cflags = os.O_RDONLY
		// This must be first since it writes the flag, not just ors into it
		if flags&keybase1.OpenFlags_WRITE != 0 {
			cflags = os.O_RDWR
		}
		if flags&keybase1.OpenFlags_EXISTING == 0 {
			cflags |= os.O_CREATE
		}
		if flags&keybase1.OpenFlags_REPLACE != 0 {
			cflags |= os.O_TRUNC
		}
		if (cflags&os.O_CREATE != 0) && (flags&keybase1.OpenFlags_DIRECTORY != 0) {
			// Return value is ignored in case the directory already
			// exists.
			os.Mkdir(path.Local(), 0755)
			return &localIO{nil, keybase1.DirentType_DIR}, nil
		}
		f, err := os.OpenFile(path.Local(), cflags, 0644)
		k.log.CDebugf(ctx, "Local open %q -> %v,%v", path.Local(), f, err)
		if err != nil {
			return nil, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		var det keybase1.DirentType
		mode := st.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			det = keybase1.DirentType_SYM
		case mode.IsDir():
			det = keybase1.DirentType_DIR
		case mode&0100 == 0100:
			det = keybase1.DirentType_EXEC
		default:
			det = keybase1.DirentType_FILE
		}
		return &localIO{f, det}, nil
	}
	return nil, simpleFSError{"Invalid path type"}
}

type kbfsIO struct {
	ctx    context.Context
	sfs    *SimpleFS
	node   libkbfs.Node
	offset int64
	deType keybase1.DirentType
}

func (r *kbfsIO) Read(bs []byte) (int, error) {
	n, err := r.sfs.config.KBFSOps().Read(r.ctx, r.node, bs, r.offset)
	if n == 0 && err == nil {
		return 0, io.EOF
	}
	r.offset += n
	return int(n), err
}

func (r *kbfsIO) Write(bs []byte) (int, error) {
	err := r.sfs.config.KBFSOps().Write(r.ctx, r.node, bs, r.offset)
	r.offset += int64(len(bs))
	return len(bs), err
}

func (r *kbfsIO) Close() error {
	return r.sfs.config.KBFSOps().SyncAll(r.ctx, r.node.GetFolderBranch())
}

func (r *kbfsIO) Type() keybase1.DirentType {
	return r.deType
}

func (r *kbfsIO) Children() (map[string]libkbfs.EntryInfo, error) {
	return r.sfs.config.KBFSOps().GetDirChildren(r.ctx, r.node)
}

type localIO struct {
	*os.File
	deType keybase1.DirentType
}

func (r *localIO) Type() keybase1.DirentType { return r.deType }
func (r *localIO) Children() (map[string]libkbfs.EntryInfo, error) {
	fis, err := r.File.Readdir(-1)
	if err != nil {
		return nil, err
	}
	eis := make(map[string]libkbfs.EntryInfo, len(fis))
	for _, fi := range fis {
		eis[fi.Name()] = libkbfs.EntryInfo{
			Type: ty2Kbfs(fi.Mode()),
		}
	}
	return eis, nil
}

func ty2Kbfs(mode os.FileMode) libkbfs.EntryType {
	switch {
	case mode.IsDir():
		return libkbfs.Dir
	case mode&os.ModeSymlink == os.ModeSymlink:
		return libkbfs.Sym
	case mode.IsRegular() && (mode&0700 == 0700):
		return libkbfs.Exec
	}
	return libkbfs.File
}

func (k *SimpleFS) startAsync(
	ctx context.Context, opid keybase1.OpID, desc keybase1.OpDescription,
	callback func(context.Context) error) error {
	ctxAsync, e0 := k.startOp(context.Background(), opid, desc)
	if e0 != nil {
		return e0
	}
	// Bind the old context to the new context, for debugging purposes.
	k.log.CDebugf(ctx, "Launching new async operation with SFSID=%s",
		ctxAsync.Value(ctxIDKey))
	go func() (err error) {
		defer func() { k.doneOp(ctxAsync, opid, err) }()
		return callback(ctxAsync)
	}()
	return nil
}

func (k *SimpleFS) startOp(ctx context.Context, opid keybase1.OpID,
	desc keybase1.OpDescription) (context.Context, error) {
	ctx = k.makeContext(ctx)
	ctx, cancel := context.WithCancel(ctx)
	k.lock.Lock()
	k.inProgress[opid] = &inprogress{desc, cancel, make(chan error, 1)}
	k.lock.Unlock()
	// ignore error, this is just for logging.
	descBS, _ := json.Marshal(desc)
	k.log.CDebugf(ctx, "start %X %s", opid, descBS)
	return k.startOpWrapContext(ctx)
}
func (k *SimpleFS) startSyncOp(ctx context.Context, name string, logarg interface{}) (context.Context, error) {
	ctx = k.makeContext(ctx)
	k.log.CDebugf(ctx, "start sync %s %v", name, logarg)
	return k.startOpWrapContext(ctx)
}
func (k *SimpleFS) startOpWrapContext(outer context.Context) (context.Context, error) {
	return libkbfs.NewContextWithCancellationDelayer(libkbfs.NewContextReplayable(
		outer, func(c context.Context) context.Context {
			return c
		}))
}

func (k *SimpleFS) doneOp(ctx context.Context, opid keybase1.OpID, err error) {
	k.lock.Lock()
	w, ok := k.inProgress[opid]
	k.lock.Unlock()
	if ok {
		w.done <- err
		close(w.done)
	}
	k.log.CDebugf(ctx, "done op %X, status=%v", opid, err)
	if ctx != nil {
		libkbfs.CleanupCancellationDelayer(ctx)
	}
}

func (k *SimpleFS) doneSyncOp(ctx context.Context, err error) {
	k.log.CDebugf(ctx, "done sync op, status=%v", err)
	if ctx != nil {
		libkbfs.CleanupCancellationDelayer(ctx)
	}
}

func (k *SimpleFS) setResult(opid keybase1.OpID, val interface{}) {
	k.lock.Lock()
	k.handles[opid] = &handle{async: val}
	k.lock.Unlock()
}

var errOnlyRemotePathSupported = simpleFSError{"Only remote paths are supported for this operation"}
var errInvalidRemotePath = simpleFSError{"Invalid remote path"}
var errNoSuchHandle = simpleFSError{"No such handle"}
var errNoResult = simpleFSError{"Async result not found"}

// simpleFSError wraps errors for SimpleFS
type simpleFSError struct {
	reason string
}

// Error implements the error interface for simpleFSError
func (e simpleFSError) Error() string { return e.reason }

// ToStatus implements the keybase1.ToStatusAble interface for simpleFSError
func (e simpleFSError) ToStatus() keybase1.Status {
	return keybase1.Status{
		Name: e.reason,
		Code: int(keybase1.StatusCode_SCGeneric),
		Desc: e.Error(),
	}
}
