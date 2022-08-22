package mount

import (
	"io"
	"net/http"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// CopyFileRange copies data from one file to another from and to specified offsets.
//
// See https://man7.org/linux/man-pages/man2/copy_file_range.2.html
// See https://github.com/libfuse/libfuse/commit/fe4f9428fc403fa8b99051f52d84ea5bd13f3855
/**
 * Copy a range of data from one file to another
 *
 * Niels de Vos: • libfuse: add copy_file_range() support
 *
 * Performs an optimized copy between two file descriptors without the
 * additional cost of transferring data through the FUSE kernel module
 * to user space (glibc) and then back into the FUSE filesystem again.
 *
 * In case this method is not implemented, applications are expected to
 * fall back to a regular file copy.   (Some glibc versions did this
 * emulation automatically, but the emulation has been removed from all
 * glibc release branches.)
 */
func (wfs *WFS) CopyFileRange(cancel <-chan struct{}, in *fuse.CopyFileRangeIn) (written uint32, code fuse.Status) {
	// flags must equal 0 for this syscall as of now
	if in.Flags != 0 {
		return 0, fuse.EINVAL
	}

	// files must exist
	fhOut := wfs.GetHandle(FileHandleId(in.FhOut))
	if fhOut == nil {
		return 0, fuse.EBADF
	}
	fhIn := wfs.GetHandle(FileHandleId(in.FhIn))
	if fhIn == nil {
		return 0, fuse.EBADF
	}

	// lock source and target file handles
	fhOut.Lock()
	defer fhOut.Unlock()
	fhOut.entryLock.Lock()
	defer fhOut.entryLock.Unlock()

	if fhOut.entry == nil {
		return 0, fuse.ENOENT
	}

	if fhIn.fh != fhOut.fh {
		fhIn.Lock()
		defer fhIn.Unlock()
		fhIn.entryLock.Lock()
		defer fhIn.entryLock.Unlock()
	}

	// directories are not supported
	if fhIn.entry.IsDirectory || fhOut.entry.IsDirectory {
		return 0, fuse.EISDIR
	}

	// cannot copy data to an overlapping range of the same file
	offInEnd := in.OffIn + in.Len - 1
	offOutEnd := in.OffOut + in.Len - 1

	if fhIn.inode == fhOut.inode && in.OffIn <= offOutEnd && offInEnd >= in.OffOut {
		return 0, fuse.EINVAL
	}

	glog.V(4).Infof(
		"CopyFileRange %s fhIn %d -> %s fhOut %d, %d:%d -> %d:%d",
		fhIn.FullPath(), fhIn.fh,
		fhOut.FullPath(), fhOut.fh,
		in.OffIn, offInEnd,
		in.OffOut, offOutEnd,
	)

	// read data from source file
	fhIn.lockForRead(int64(in.OffIn), int(in.Len))
	defer fhIn.unlockForRead(int64(in.OffIn), int(in.Len))

	data := make([]byte, int(in.Len))
	totalRead, err := fhIn.readFromChunks(data, int64(in.OffIn))
	if err == nil || err == io.EOF {
		maxStop := fhIn.readFromDirtyPages(data, int64(in.OffIn))
		totalRead = max(maxStop-int64(in.OffIn), totalRead)
	}
	if err == io.EOF {
		err = nil
	}
	if err != nil {
		glog.Warningf("file handle read %s %d: %v", fhIn.FullPath(), totalRead, err)
		return 0, fuse.EIO
	}

	if totalRead == 0 {
		return 0, fuse.OK
	}

	// put data at the specified offset in target file
	fhOut.dirtyPages.writerPattern.MonitorWriteAt(int64(in.OffOut), int(in.Len))
	fhOut.entry.Content = nil
	fhOut.dirtyPages.AddPage(int64(in.OffOut), data, fhOut.dirtyPages.writerPattern.IsSequentialMode())
	fhOut.entry.Attributes.FileSize = uint64(max(int64(in.OffOut)+totalRead, int64(fhOut.entry.Attributes.FileSize)))
	fhOut.dirtyMetadata = true
	written = uint32(totalRead)

	// detect mime type
	if written > 0 && in.OffOut <= 512 {
		fhOut.contentType = http.DetectContentType(data[:min(totalRead, 512)-1])
	}

	return written, fuse.OK
}