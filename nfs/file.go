// Copyright © 2017 VMware, Inc. All Rights Reserved.
// SPDX-License-Identifier: BSD-2-Clause
//
package nfs

import (
	"errors"
	"io"
	"os"

	"github.com/go-nfs/nfsv3/nfs/rpc"
	"github.com/go-nfs/nfsv3/nfs/util"
	"github.com/go-nfs/nfsv3/nfs/xdr"
)

// File wraps the NfsProc3Read and NfsProc3Write methods to implement a
// io.ReadWriteCloser.
type File struct {
	*Target

	// current position
	curr   uint64
	fattr  *Fattr
	fsinfo *FSInfo

	// filehandle to the file
	fh []byte
}

// Readlink gets the target of a symlink
func (f *File) Readlink() (string, error) {
	type ReadlinkArgs struct {
		rpc.Header
		FH []byte
	}

	type ReadlinkRes struct {
		Attr PostOpAttr
		data []byte
	}

	r, err := f.call(&ReadlinkArgs{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3Readlink,
			Cred:    f.auth,
			Verf:    rpc.AuthNull,
		},
		FH: f.fh,
	})

	if err != nil {
		util.Debugf("readlink(%x): %s", f.fh, err.Error())
		return "", err
	}

	readlinkres := &ReadlinkRes{}
	if err = xdr.Read(r, readlinkres); err != nil {
		return "", err
	}

	if readlinkres.data, err = xdr.ReadOpaque(r); err != nil {
		return "", err
	}

	return string(readlinkres.data), err
}

func (f *File) Read(p []byte) (int, error) {
	type ReadArgs struct {
		rpc.Header
		FH     []byte
		Offset uint64
		Count  uint32
	}

	type ReadRes struct {
		Attr  PostOpAttr
		Count uint32
		EOF   uint32
		Data  struct {
			Length uint32
		}
	}

	readSize := min(f.fsinfo.RTPref, uint32(len(p)))
	util.Debugf("read(%x) len=%d offset=%d", f.fh, readSize, f.curr)

	r, err := f.call(&ReadArgs{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3Read,
			Cred:    f.auth,
			Verf:    rpc.AuthNull,
		},
		FH:     f.fh,
		Offset: uint64(f.curr),
		Count:  readSize,
	})

	if err != nil {
		util.Debugf("read(%x): %s", f.fh, err.Error())
		return 0, err
	}

	readres := &ReadRes{}
	if err = xdr.Read(r, readres); err != nil {
		return 0, err
	}

	f.curr = f.curr + uint64(readres.Data.Length)
	n, err := r.Read(p[:readres.Data.Length])
	if err != nil {
		return n, err
	}

	if readres.EOF != 0 {
		err = io.EOF
	}

	return n, err
}

func (f *File) Write(p []byte) (int, error) {
	type WriteArgs struct {
		rpc.Header
		FH     []byte
		Offset uint64
		Count  uint32

		// UNSTABLE(0), DATA_SYNC(1), FILE_SYNC(2) default
		How      uint32
		Contents []byte
	}

	type WriteRes struct {
		Wcc       WccData
		Count     uint32
		How       uint32
		WriteVerf uint64
	}

	totalToWrite := uint32(len(p))
	written := uint32(0)

	for written = 0; written < totalToWrite; {
		writeSize := min(f.fsinfo.WTPref, totalToWrite-written)

		res, err := f.call(&WriteArgs{
			Header: rpc.Header{
				Rpcvers: 2,
				Prog:    Nfs3Prog,
				Vers:    Nfs3Vers,
				Proc:    NFSProc3Write,
				Cred:    f.auth,
				Verf:    rpc.AuthNull,
			},
			FH:       f.fh,
			Offset:   f.curr,
			Count:    writeSize,
			How:      2,
			Contents: p[written : written+writeSize],
		})

		if err != nil {
			util.Errorf("write(%x): %s", f.fh, err.Error())
			return int(written), err
		}

		writeres := &WriteRes{}
		if err = xdr.Read(res, writeres); err != nil {
			util.Errorf("write(%x) failed to parse result: %s", f.fh, err.Error())
			util.Debugf("write(%x) partial result: %+v", f.fh, writeres)
			return int(written), err
		}

		if writeres.Count != writeSize {
			util.Debugf("write(%x) did not write full data payload: sent: %d, written: %d", writeSize, writeres.Count)
		}

		f.curr += uint64(writeres.Count)
		written += writeres.Count

		util.Debugf("write(%x) len=%d new_offset=%d written=%d total=%d", f.fh, totalToWrite, f.curr, writeres.Count, written)
	}

	return int(written), nil
}

// Close commits the file
func (f *File) Close() error {
	type CommitArg struct {
		rpc.Header
		FH     []byte
		Offset uint64
		Count  uint32
	}

	_, err := f.call(&CommitArg{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3Commit,
			Cred:    f.auth,
			Verf:    rpc.AuthNull,
		},
		FH: f.fh,
	})

	if err != nil {
		util.Debugf("commit(%x): %s", f.fh, err.Error())
		return err
	}

	return nil
}

// Seek sets the offset for the next Read or Write to offset, interpreted according to whence.
// This method implements Seeker interface.
func (f *File) Seek(offset int64, whence int) (int64, error) {

	// It would be nice to try to validate the offset here.
	// However, as we're working with the shared file system, the file
	// size might even change between NFSPROC3_GETATTR call and
	// Seek() call, so don't even try to validate it.
	switch whence {
	case io.SeekStart:
		if offset < 0 {
			return int64(f.curr), errors.New("offset cannot be negative")
		}
		f.curr = uint64(offset)
		return int64(f.curr), nil
	case io.SeekCurrent:
		f.curr = uint64(int64(f.curr) + offset)
		return int64(f.curr), nil
	case io.SeekEnd:
		if f.curr < f.fattr.Filesize {
			f.curr = f.fattr.Filesize
		}
		return int64(f.curr), nil
	default:
		// This indicates serious programming error
		return int64(f.curr), errors.New("Invalid whence")
	}
}

// OpenFile writes to an existing file or creates one
func (v *Target) OpenFile(path string, perm os.FileMode) (*File, error) {
	_, fh, err := v.Lookup(path)
	if err != nil {
		if os.IsNotExist(err) {
			fh, err = v.Create(path, perm)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	f := &File{
		Target: v,
		fsinfo: v.fsinfo,
		fh:     fh,
	}

	return f, nil
}

// Open opens a file for reading
func (v *Target) Open(path string) (*File, error) {
	fattr, fh, _, _, err := v.lookupInner(v.fh, path, true, nil)
	if err != nil {
		return nil, err
	}

	f := &File{
		Target: v,
		fsinfo: v.fsinfo,
		fattr:  fattr,
		fh:     fh,
	}

	return f, nil
}

// OpenByFh opens a file using file handle instead of path
func (v *Target) OpenByFh(fh []byte, fattr *Fattr) (*File, error) {
	f := &File{
		Target: v,
		fsinfo: v.fsinfo,
		fattr:  fattr,
		fh:     fh,
	}

	return f, nil
}

// Symlink creates a symlink as where pointing to symlink
func (v *Target) Symlink(where, symlink string) (*File, error) {
	type symlinkdata3 struct {
		SymlinkAttr Sattr3
		SymlinkData []byte
	}

	type SymlinkArgs struct {
		rpc.Header
		Where   Diropargs3
		Symlink symlinkdata3
	}

	type SymlinkRes struct {
		obj     PostOpFH3
		objAttr PostOpAttr
		Wcc     WccData
	}

	_, _, symlinkName, fh, err := v.lookupInner(v.fh, symlink, false, nil)
	if err != nil {
		return nil, err
	}

	r, err := v.call(&SymlinkArgs{
		Header: rpc.Header{
			Rpcvers: 2,
			Prog:    Nfs3Prog,
			Vers:    Nfs3Vers,
			Proc:    NFSProc3Symlink,
			Cred:    v.auth,
			Verf:    rpc.AuthNull,
		},
		Where: Diropargs3{
			FH:       fh,
			Filename: symlinkName,
		},
		Symlink: symlinkdata3{
			SymlinkAttr: Sattr3{},
			SymlinkData: []byte(symlink),
		},
	})

	if err != nil {
		util.Debugf("Symlink(%s): %s", where, err.Error())
		return nil, err
	}

	symlinkres := &SymlinkRes{}
	if err = xdr.Read(r, symlinkres); err != nil {
		return nil, err
	}

	if !symlinkres.obj.IsSet {
		return nil, errors.New("fh not set")
	}

	symFile := &File{
		Target: v,
		fsinfo: v.fsinfo,
		fh:     symlinkres.obj.FH,
	}

	return symFile, nil
}

func min(x, y uint32) uint32 {
	if x > y {
		return y
	}
	return x
}
