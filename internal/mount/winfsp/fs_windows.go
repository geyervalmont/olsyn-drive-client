//go:build windows

package winfsp

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/geyervalmont/olsyn-drive-client/internal/vfs"
	"github.com/winfsp/go-winfsp"
	"golang.org/x/sys/windows"
)

const (
	allocUnit  = 4096 // matches SectorSize(512, 8)
	dirInfoMax = int(unsafe.Sizeof(winfsp.FSP_FSCTL_DIR_INFO{})) + 260*2
	listPage   = 1000
)

// handle is one WinFsp file context.
type handle struct {
	h    *vfs.Handle
	mu   sync.Mutex
	path string // WinFsp name, updated by Rename through this context
	dir  winfsp.DirBuffer
}

func (hd *handle) name() string {
	hd.mu.Lock()
	defer hd.mu.Unlock()
	return hd.path
}

type adapter struct {
	ctx         context.Context
	fs          vfs.FileSystem
	log         *slog.Logger
	markOffline bool
	sdWritable  *windows.SECURITY_DESCRIPTOR
	sdReadOnly  *windows.SECURITY_DESCRIPTOR

	handles sync.Map // uintptr -> *handle
	nextID  atomic.Uintptr
	// closing counts Closes still finishing off the dispatcher threads.
	closing sync.WaitGroup
}

func newAdapter(ctx context.Context, fs vfs.FileSystem, markOffline bool, log *slog.Logger) (*adapter, error) {
	a := &adapter{ctx: ctx, fs: fs, log: log, markOffline: markOffline}
	var err error
	// The drive decides permissions; Windows only needs to know what is
	// read-only so the kernel refuses writes there before asking us.
	if a.sdWritable, err = windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;;FA;;;WD)"); err != nil {
		return nil, err
	}
	if a.sdReadOnly, err = windows.SecurityDescriptorFromString("O:BAG:BAD:P(A;;FRFX;;;WD)"); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *adapter) closeAll() {
	a.handles.Range(func(k, v any) bool {
		hd := v.(*handle)
		_ = hd.h.Close()
		hd.dir.Delete()
		a.handles.Delete(k)
		return true
	})
	a.closing.Wait()
}

func (a *adapter) register(hd *handle) uintptr {
	id := a.nextID.Add(1) // starts at 1: context 0 means "the volume" in Flush
	a.handles.Store(id, hd)
	return id
}

func (a *adapter) get(file uintptr) *handle {
	v, ok := a.handles.Load(file)
	if !ok {
		return nil
	}
	return v.(*handle)
}

// guard converts the callback's error to an NTSTATUS, logs failures that
// are not plain "not found", and turns a panic into STATUS_INTERNAL_ERROR (a
// panic inside a WinFsp callback would take the drive down).
// Use as: defer a.guard(op, name, &err)()
func (a *adapter) guard(op string, name *string, err *error) func() {
	return func() {
		if p := recover(); p != nil {
			*err = windows.STATUS_INTERNAL_ERROR
			a.log.Error("winfsp: panic in callback", "op", op, "panic", p, "stack", string(debug.Stack()))
			return
		}
		if *err == nil {
			return
		}
		st := toStatus(*err)
		if st != windows.STATUS_OBJECT_NAME_NOT_FOUND && st != windows.STATUS_OBJECT_PATH_NOT_FOUND {
			a.log.Debug("winfsp: callback failed", "op", op, "path", *name, "status", uint32(st), "err", *err)
		}
		*err = st
	}
}

// ---- conversions ----

func toFiletime(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	ft := windows.NsecToFiletime(t.UnixNano())
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

func fromFiletime(v uint64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	ft := windows.Filetime{LowDateTime: uint32(v), HighDateTime: uint32(v >> 32)}
	return time.Unix(0, ft.Nanoseconds()).UTC()
}

func indexNumber(id string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return h.Sum64()
}

func (a *adapter) attrs(in vfs.Info) uint32 {
	at := in.Attrs &^ (windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_OFFLINE |
		windows.FILE_ATTRIBUTE_READONLY | windows.FILE_ATTRIBUTE_NORMAL | windows.FILE_ATTRIBUTE_REPARSE_POINT)
	if in.IsDir {
		at |= windows.FILE_ATTRIBUTE_DIRECTORY
	} else {
		if in.ReadOnly {
			at |= windows.FILE_ATTRIBUTE_READONLY
		}
		if in.Offline && a.markOffline {
			at |= windows.FILE_ATTRIBUTE_OFFLINE
		}
	}
	if at == 0 {
		at = windows.FILE_ATTRIBUTE_NORMAL
	}
	return at
}

func (a *adapter) fill(in vfs.Info, fi *winfsp.FSP_FSCTL_FILE_INFO) {
	size := uint64(max(in.Size, 0))
	mt := toFiletime(in.MTime)
	ct := toFiletime(in.CTime)
	if ct == 0 {
		ct = mt
	}
	*fi = winfsp.FSP_FSCTL_FILE_INFO{
		FileAttributes: a.attrs(in),
		FileSize:       size,
		AllocationSize: (size + allocUnit - 1) / allocUnit * allocUnit,
		CreationTime:   ct,
		LastAccessTime: mt,
		LastWriteTime:  mt,
		ChangeTime:     mt,
		IndexNumber:    indexNumber(in.ID),
	}
}

func (a *adapter) sd(in vfs.Info) *windows.SECURITY_DESCRIPTOR {
	if in.ReadOnly {
		return a.sdReadOnly
	}
	return a.sdWritable
}

func access(granted uint32) vfs.Access {
	var acc vfs.Access
	if granted&(windows.FILE_READ_DATA|windows.GENERIC_READ|windows.GENERIC_ALL|windows.MAXIMUM_ALLOWED) != 0 {
		acc |= vfs.AccessRead
	}
	if granted&(windows.FILE_WRITE_DATA|windows.FILE_APPEND_DATA|windows.GENERIC_WRITE|windows.GENERIC_ALL) != 0 {
		acc |= vfs.AccessWrite
	}
	if granted&(windows.DELETE|windows.GENERIC_ALL) != 0 {
		acc |= vfs.AccessDelete
	}
	return acc
}

// setNormalized reports the stored case of an opened name. WinFsp passes
// &FSP_FSCTL_OPEN_FILE_INFO.FileInfo to Create and Open, so the pointer may
// be widened (this is FspFileSystemGetOpenFileInfo).
func setNormalized(info *winfsp.FSP_FSCTL_FILE_INFO, name, stored string) {
	if stored == "" || name == "" || name == "\\" {
		return
	}
	i := strings.LastIndexByte(name, '\\')
	full := name[:i+1] + stored
	if full == name {
		return
	}
	ofi := (*winfsp.FSP_FSCTL_OPEN_FILE_INFO)(unsafe.Pointer(info))
	if ofi.NormalizedName == nil {
		return
	}
	u := utf16.Encode([]rune(full))
	if len(u)*2 > int(ofi.NormalizedNameSize) {
		return
	}
	copy(unsafe.Slice(ofi.NormalizedName, len(u)), u)
	ofi.NormalizedNameSize = uint16(len(u) * 2)
}

// ---- callbacks ----

func (a *adapter) GetVolumeInfo(_ *winfsp.FileSystemRef, info *winfsp.FSP_FSCTL_VOLUME_INFO) (err error) {
	name := ""
	defer a.guard("GetVolumeInfo", &name, &err)()
	v := a.fs.VolumeInfo(a.ctx)
	info.TotalSize = uint64(max(v.Total, 0))
	info.FreeSize = uint64(max(v.Free, 0))
	label := utf16.Encode([]rune(v.Label))
	n := copy(info.VolumeLabel[:], label)
	info.VolumeLabelLength = uint16(n * 2)
	return nil
}

func (a *adapter) GetSecurityByName(_ *winfsp.FileSystemRef, name string, flags winfsp.GetSecurityByNameFlags) (attr uint32, sd *windows.SECURITY_DESCRIPTOR, err error) {
	defer a.guard("GetSecurityByName", &name, &err)()
	in, err := a.fs.Stat(a.ctx, name)
	if err != nil {
		return 0, nil, err
	}
	// go-winfsp calls sd.Length() whenever the caller asked for the
	// descriptor, so it is never nil on success.
	return a.attrs(in), a.sd(in), nil
}

func (a *adapter) GetSecurity(_ *winfsp.FileSystemRef, file uintptr) (sd *windows.SECURITY_DESCRIPTOR, err error) {
	name := ""
	defer a.guard("GetSecurity", &name, &err)()
	hd := a.get(file)
	if hd == nil {
		return nil, windows.STATUS_INVALID_HANDLE
	}
	return a.sd(hd.h.Info()), nil
}

func (a *adapter) Create(_ *winfsp.FileSystemRef, name string, createOptions, grantedAccess, fileAttributes uint32,
	_ *windows.SECURITY_DESCRIPTOR, _ uint64, info *winfsp.FSP_FSCTL_FILE_INFO) (file uintptr, err error) {
	defer a.guard("Create", &name, &err)()
	isDir := createOptions&windows.FILE_DIRECTORY_FILE != 0
	h, err := a.fs.Open(a.ctx, name, vfs.OpenFlags{
		Create: true, Exclusive: true, Directory: isDir, NonDirectory: !isDir,
		// WinFsp's kernel driver enforces share modes; vfs need not repeat it
		Access: access(grantedAccess) | vfs.AccessRead, Share: vfs.ShareAll,
		Attrs: fileAttributes &^ (windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_NORMAL),
	})
	if err != nil {
		return 0, err
	}
	in := h.Info()
	a.fill(in, info)
	setNormalized(info, name, in.Name)
	return a.register(&handle{h: h, path: name}), nil
}

func (a *adapter) Open(_ *winfsp.FileSystemRef, name string, createOptions, grantedAccess uint32,
	info *winfsp.FSP_FSCTL_FILE_INFO) (file uintptr, err error) {
	defer a.guard("Open", &name, &err)()
	fl := vfs.OpenFlags{
		Directory:    createOptions&windows.FILE_DIRECTORY_FILE != 0,
		NonDirectory: createOptions&windows.FILE_NON_DIRECTORY_FILE != 0,
		Access:       access(grantedAccess) | vfs.AccessRead | vfs.AccessWrite, Share: vfs.ShareAll,
	}
	// Ask for more than granted (the kernel driver already checked what the
	// caller may do): WinFsp can send cached (paging) writes and attribute
	// changes through any open of the file, not only the one that wrote.
	// Read-only files fall back to what was granted.
	h, err := a.fs.Open(a.ctx, name, fl)
	if vfs.KindOf(err) == vfs.KindAccessDenied {
		fl.Access = access(grantedAccess)
		h, err = a.fs.Open(a.ctx, name, fl)
	}
	if err != nil {
		return 0, err
	}
	in := h.Info()
	a.fill(in, info)
	setNormalized(info, name, in.Name)
	return a.register(&handle{h: h, path: name}), nil
}

func (a *adapter) Overwrite(_ *winfsp.FileSystemRef, file uintptr, attributes uint32, replace bool,
	_ uint64, info *winfsp.FSP_FSCTL_FILE_INFO) (err error) {
	name := ""
	defer a.guard("Overwrite", &name, &err)()
	hd := a.get(file)
	if hd == nil {
		return windows.STATUS_INVALID_HANDLE
	}
	name = hd.name()
	attributes &^= windows.FILE_ATTRIBUTE_OFFLINE | windows.FILE_ATTRIBUTE_NORMAL
	if !replace {
		attributes |= hd.h.Info().Attrs
	}
	if err := hd.h.Truncate(a.ctx, 0); err != nil {
		return err
	}
	if err := hd.h.SetAttrs(a.ctx, attributes); err != nil {
		return err
	}
	a.fill(hd.h.Info(), info)
	return nil
}

func (a *adapter) Cleanup(_ *winfsp.FileSystemRef, file uintptr, name string, flags uint32) {
	var err error
	defer a.guard("Cleanup", &name, &err)()
	hd := a.get(file)
	if hd == nil {
		return
	}
	// The last cleanup of a file whose delete disposition is set (CanDelete
	// said yes, or FILE_DELETE_ON_CLOSE) carries FspCleanupDelete.
	err = hd.h.Cleanup(a.ctx, flags&winfsp.FspCleanupDelete != 0)
}

func (a *adapter) Close(_ *winfsp.FileSystemRef, file uintptr) {
	var err error
	name := ""
	defer a.guard("Close", &name, &err)()
	v, ok := a.handles.LoadAndDelete(file)
	if !ok {
		return
	}
	hd := v.(*handle)
	// vfs Close waits for the journal's fsync (shared by every Close in
	// flight). WinFsp has already answered the app; finishing here would
	// hold one of its few dispatcher threads for that fsync, so the next
	// file's Create would wait behind it. Finish on another goroutine.
	a.closing.Add(1)
	go func() {
		defer a.closing.Done()
		if cerr := hd.h.Close(); cerr != nil {
			a.log.Warn("winfsp: close", "err", cerr)
		}
		hd.dir.Delete()
	}()
}

func (a *adapter) Read(_ *winfsp.FileSystemRef, file uintptr, buf []byte, offset uint64) (n int, err error) {
	name := ""
	defer a.guard("Read", &name, &err)()
	hd := a.get(file)
	if hd == nil {
		return 0, windows.STATUS_INVALID_HANDLE
	}
	n, err = hd.h.ReadAt(a.ctx, buf, int64(offset))
	if errors.Is(err, io.EOF) {
		if n > 0 {
			return n, nil
		}
		return 0, windows.STATUS_END_OF_FILE
	}
	if err != nil {
		name = hd.name()
	}
	return n, err
}

func (a *adapter) Write(_ *winfsp.FileSystemRef, file uintptr, buf []byte, offset uint64,
	writeToEndOfFile, constrainedIo bool, info *winfsp.FSP_FSCTL_FILE_INFO) (n int, err error) {
	name := ""
	defer a.guard("Write", &name, &err)()
	hd := a.get(file)
	if hd == nil {
		return 0, windows.STATUS_INVALID_HANDLE
	}
	off := int64(offset)
	size := hd.h.Info().Size
	if writeToEndOfFile {
		off = size
	}
	if constrainedIo {
		// paging write: never extend the file
		if off >= size {
			a.fill(hd.h.Info(), info)
			return 0, nil
		}
		buf = buf[:min(int64(len(buf)), size-off)]
	}
	n, err = hd.h.WriteAt(a.ctx, buf, off)
	if err != nil {
		name = hd.name()
		return n, err
	}
	a.fill(hd.h.Info(), info)
	return n, nil
}

func (a *adapter) Flush(_ *winfsp.FileSystemRef, file uintptr, info *winfsp.FSP_FSCTL_FILE_INFO) (err error) {
	name := ""
	defer a.guard("Flush", &name, &err)()
	if file == 0 {
		return nil // the whole volume: the cache flushes on its own
	}
	hd := a.get(file)
	if hd == nil {
		return windows.STATUS_INVALID_HANDLE
	}
	if err := hd.h.Flush(a.ctx); err != nil {
		name = hd.name()
		return err
	}
	a.fill(hd.h.Info(), info)
	return nil
}

func (a *adapter) GetFileInfo(_ *winfsp.FileSystemRef, file uintptr, info *winfsp.FSP_FSCTL_FILE_INFO) (err error) {
	name := ""
	defer a.guard("GetFileInfo", &name, &err)()
	hd := a.get(file)
	if hd == nil {
		return windows.STATUS_INVALID_HANDLE
	}
	a.fill(hd.h.Info(), info)
	return nil
}

func (a *adapter) SetBasicInfo(_ *winfsp.FileSystemRef, file uintptr, flags winfsp.SetBasicInfoFlags, attributes uint32,
	creationTime, _, lastWriteTime, _ uint64, info *winfsp.FSP_FSCTL_FILE_INFO) (err error) {
	name := ""
	defer a.guard("SetBasicInfo", &name, &err)()
	hd := a.get(file)
	if hd == nil {
		return windows.STATUS_INVALID_HANDLE
	}
	name = hd.name()
	if flags&winfsp.SetBasicInfoAttributes != 0 {
		at := attributes &^ (windows.FILE_ATTRIBUTE_DIRECTORY | windows.FILE_ATTRIBUTE_OFFLINE | windows.FILE_ATTRIBUTE_NORMAL)
		if err := hd.h.SetAttrs(a.ctx, at); err != nil {
			return err
		}
	}
	// zero means "leave unchanged"
	if lastWriteTime != 0 || creationTime != 0 {
		if err := hd.h.SetTimes(a.ctx, fromFiletime(lastWriteTime), fromFiletime(creationTime)); err != nil {
			return err
		}
	}
	a.fill(hd.h.Info(), info)
	return nil
}

func (a *adapter) SetFileSize(_ *winfsp.FileSystemRef, file uintptr, newSize uint64, setAllocationSize bool,
	info *winfsp.FSP_FSCTL_FILE_INFO) (err error) {
	name := ""
	defer a.guard("SetFileSize", &name, &err)()
	hd := a.get(file)
	if hd == nil {
		return windows.STATUS_INVALID_HANDLE
	}
	name = hd.name()
	size := int64(newSize)
	// the allocation size only matters when it cuts below the file size
	if !setAllocationSize || size < hd.h.Info().Size {
		if err := hd.h.Truncate(a.ctx, size); err != nil {
			return err
		}
	}
	a.fill(hd.h.Info(), info)
	return nil
}

// CanDelete only checks; the delete happens in Cleanup.
func (a *adapter) CanDelete(_ *winfsp.FileSystemRef, file uintptr, name string) (err error) {
	defer a.guard("CanDelete", &name, &err)()
	hd := a.get(file)
	if hd == nil {
		return windows.STATUS_INVALID_HANDLE
	}
	return hd.h.CanDelete(a.ctx)
}

func (a *adapter) Rename(_ *winfsp.FileSystemRef, file uintptr, source, target string, replaceIfExists bool) (err error) {
	defer a.guard("Rename", &source, &err)()
	hd := a.get(file)
	if hd == nil {
		return windows.STATUS_INVALID_HANDLE
	}
	if err := a.fs.Rename(a.ctx, source, target, replaceIfExists); err != nil {
		return err
	}
	hd.mu.Lock()
	hd.path = target
	hd.mu.Unlock()
	return nil
}

// ReadDirectoryRaw is WinFsp's DirBuffer pattern: the buffer is filled once
// per listing (marker nil) and continuations are served from it.
func (a *adapter) ReadDirectoryRaw(_ *winfsp.FileSystemRef, file uintptr, _, marker *uint16, buf []byte) (n int, err error) {
	name := ""
	defer a.guard("ReadDirectory", &name, &err)()
	hd := a.get(file)
	if hd == nil {
		return 0, windows.STATUS_INVALID_HANDLE
	}
	name = hd.name()
	if !hd.h.Info().IsDir {
		return 0, windows.STATUS_NOT_A_DIRECTORY
	}
	filler, err := hd.dir.Acquire(marker == nil)
	if err != nil {
		return 0, err
	}
	if filler != nil {
		ferr := func() error {
			defer filler.Release()
			return a.fillDir(hd, name, filler.Fill)
		}()
		if ferr != nil {
			return 0, ferr
		}
	}
	return hd.dir.ReadDirectory(marker, buf), nil
}

func (a *adapter) fillDir(hd *handle, name string, fill func(string, *winfsp.FSP_FSCTL_FILE_INFO) (bool, error)) error {
	var fi winfsp.FSP_FSCTL_FILE_INFO
	if name != "\\" && name != "" {
		a.fill(hd.h.Info(), &fi)
		for _, dot := range []string{".", ".."} {
			if ok, err := fill(dot, &fi); err != nil || !ok {
				return err
			}
		}
	}
	marker := ""
	for {
		page, next, err := hd.h.ReadDir(a.ctx, marker, listPage)
		if err != nil {
			return err
		}
		for _, in := range page {
			a.fill(in, &fi)
			if ok, err := fill(in.Name, &fi); err != nil || !ok {
				return err
			}
		}
		if next == "" {
			return nil
		}
		marker = next
	}
}

// GetDirInfoByName answers a FindFirstFile without wildcards (enabled by
// PassQueryDirectoryFileName) so looking up one name does not list a folder.
func (a *adapter) GetDirInfoByName(_ *winfsp.FileSystemRef, parent uintptr, name string, dirInfo *winfsp.FSP_FSCTL_DIR_INFO) (err error) {
	defer a.guard("GetDirInfoByName", &name, &err)()
	hd := a.get(parent)
	if hd == nil {
		return windows.STATUS_INVALID_HANDLE
	}
	if strings.ContainsAny(name, "*?<>\"\\") {
		return windows.STATUS_OBJECT_NAME_NOT_FOUND
	}
	in, err := a.fs.Stat(a.ctx, strings.TrimSuffix(hd.name(), "\\")+"\\"+name)
	if err != nil {
		return err
	}
	var fi winfsp.FSP_FSCTL_FILE_INFO
	a.fill(in, &fi)
	// the DLL hands us a FSP_FSCTL_DIR_INFO with room for a MAX_PATH name
	out := unsafe.Slice((*byte)(unsafe.Pointer(dirInfo)), dirInfoMax)
	if winfsp.FileSystemAddDirInfo(in.Name, 0, &fi, out) == 0 {
		return windows.STATUS_OBJECT_NAME_INVALID
	}
	return nil
}

var (
	_ winfsp.BehaviourBase              = (*adapter)(nil)
	_ winfsp.BehaviourGetVolumeInfo     = (*adapter)(nil)
	_ winfsp.BehaviourGetSecurityByName = (*adapter)(nil)
	_ winfsp.BehaviourCreate            = (*adapter)(nil)
	_ winfsp.BehaviourOverwrite         = (*adapter)(nil)
	_ winfsp.BehaviourCleanup           = (*adapter)(nil)
	_ winfsp.BehaviourRead              = (*adapter)(nil)
	_ winfsp.BehaviourWrite             = (*adapter)(nil)
	_ winfsp.BehaviourFlush             = (*adapter)(nil)
	_ winfsp.BehaviourGetFileInfo       = (*adapter)(nil)
	_ winfsp.BehaviourSetBasicInfo      = (*adapter)(nil)
	_ winfsp.BehaviourSetFileSize       = (*adapter)(nil)
	_ winfsp.BehaviourCanDelete         = (*adapter)(nil)
	_ winfsp.BehaviourRename            = (*adapter)(nil)
	_ winfsp.BehaviourGetSecurity       = (*adapter)(nil)
	_ winfsp.BehaviourReadDirectoryRaw  = (*adapter)(nil)
	_ winfsp.BehaviourGetDirInfoByName  = (*adapter)(nil)
)
