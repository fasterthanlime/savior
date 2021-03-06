package savior

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"

	"github.com/itchio/headway/state"
	"github.com/itchio/ox"
	"github.com/pkg/errors"
)

var EnableLegacyPreallocate = os.Getenv("SAVIOR_LEGACY_PREALLOCATE") == "1"

const (
	// ModeMask is or'd with files walked by butler
	ModeMask = 0666

	// LuckyMode is used when wiping in last-chance mode
	LuckyMode = 0777

	// DirMode is the default mode for directories created by butler
	DirMode = 0755
)

var onWindows = runtime.GOOS == "windows"

type FolderSink struct {
	Directory string
	Consumer  *state.Consumer

	writer *entryWriter
}

var _ Sink = (*FolderSink)(nil)

var ignoredNames = map[string]struct{}{
	// the path for folder icons on macOS (yes, really).
	// thanks to Jordan Rose for pointing it out, and
	// no thanks to whoever thought of that.
	"Icon\r": struct{}{},
}

func shouldIgnorePath(s string) bool {
	_, ok := ignoredNames[path.Base(s)]
	return ok
}

func (fs *FolderSink) destPath(entry *Entry) string {
	return filepath.Join(fs.Directory, filepath.FromSlash(entry.CanonicalPath))
}

func (fs *FolderSink) Mkdir(entry *Entry) error {
	if shouldIgnorePath(entry.CanonicalPath) {
		return nil
	}

	dstpath := fs.destPath(entry)

	dirstat, err := os.Lstat(dstpath)
	if err != nil {
		// main case - dir doesn't exist yet
		err = os.MkdirAll(dstpath, DirMode)
		if err != nil {
			return errors.WithStack(err)
		}
		return nil
	}

	if dirstat.IsDir() {
		// is already a dir, good!
	} else {
		// is a file or symlink for example, turn into a dir
		err = os.Remove(dstpath)
		if err != nil {
			return errors.WithStack(err)
		}
		err = os.MkdirAll(dstpath, DirMode)
		if err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

func (fs *FolderSink) createFile(entry *Entry) (*os.File, error) {
	dstpath := fs.destPath(entry)

	dirname := filepath.Dir(dstpath)
	err := os.MkdirAll(dirname, LuckyMode)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	stats, err := os.Lstat(dstpath)
	if err == nil {
		if stats.Mode()&os.ModeSymlink > 0 {
			// if it used to be a symlink, remove it
			err = os.RemoveAll(dstpath)
			if err != nil {
				return nil, errors.WithStack(err)
			}
		}
	}

	flag := os.O_CREATE | os.O_WRONLY
	f, err := os.OpenFile(dstpath, flag, entry.Mode|ModeMask)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if stats != nil && !onWindows {
		// if file already existed, chmod it, just in case
		err = f.Chmod(entry.Mode | ModeMask)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	return f, nil
}

func (fs *FolderSink) GetWriter(entry *Entry) (EntryWriter, error) {
	if shouldIgnorePath(entry.CanonicalPath) {
		return &nopEntryWriter{}, nil
	}

	f, err := fs.createFile(entry)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if f == nil {
		return nil, nil
	}

	if entry.WriteOffset > 0 {
		_, err = f.Seek(entry.WriteOffset, io.SeekStart)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	err = f.Truncate(entry.WriteOffset)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	err = fs.Close()
	if err != nil {
		fs.Consumer.Warnf("folder_sink could not close last writer: %s", err.Error())
	}

	ew := &entryWriter{
		fs:    fs,
		f:     f,
		entry: entry,
	}
	fs.writer = ew

	return ew, nil
}

func (fs *FolderSink) Preallocate(entry *Entry) error {
	if shouldIgnorePath(entry.CanonicalPath) {
		return nil
	}

	f, err := fs.createFile(entry)
	if err != nil {
		return errors.WithStack(err)
	}

	defer f.Close()

	if entry.UncompressedSize > 0 {
		if EnableLegacyPreallocate {
			err := legacyPreallocate(f, entry.UncompressedSize)
			if err != nil {
				return err
			}
		} else {
			err := ox.Preallocate(f, entry.UncompressedSize)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func legacyPreallocate(f *os.File, size int64) error {
	endOffset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return errors.WithStack(err)
	}

	allocSize := size - endOffset
	if allocSize > 0 {
		_, err := io.CopyN(f, &zeroReader{}, allocSize)
		if err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

func (fs *FolderSink) Symlink(entry *Entry, linkname string) error {
	if shouldIgnorePath(entry.CanonicalPath) {
		return nil
	}

	if onWindows {
		// on windows, write symlinks as regular files
		w, err := fs.GetWriter(entry)
		if err != nil {
			return errors.WithStack(err)
		}
		defer w.Close()

		_, err = w.Write([]byte(linkname))
		if err != nil {
			return errors.WithStack(err)
		}

		return nil
	}

	// actual symlink code
	dstpath := fs.destPath(entry)

	err := os.RemoveAll(dstpath)
	if err != nil {
		return errors.WithStack(err)
	}

	dirname := filepath.Dir(dstpath)
	err = os.MkdirAll(dirname, LuckyMode)
	if err != nil {
		return errors.WithStack(err)
	}

	err = os.Symlink(linkname, dstpath)
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (fs *FolderSink) Nuke() error {
	err := fs.Close()
	if err != nil {
		return errors.WithStack(err)
	}

	// TODO: retry logic, a-la butler
	return os.RemoveAll(fs.Directory)
}

func (fs *FolderSink) Close() error {
	if fs.writer != nil {
		err := fs.writer.Close()
		fs.writer = nil
		return err
	}

	return nil
}

type entryWriter struct {
	fs    *FolderSink
	f     *os.File
	entry *Entry
}

var _ EntryWriter = (*entryWriter)(nil)

func (ew *entryWriter) Write(buf []byte) (int, error) {
	if ew.f == nil {
		return 0, os.ErrClosed
	}

	n, err := ew.f.Write(buf)
	ew.entry.WriteOffset += int64(n)
	return n, err
}

func (ew *entryWriter) Close() error {
	if ew.f == nil {
		// already closed
		return nil
	}

	err := ew.f.Close()
	ew.f = nil
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (ew *entryWriter) Sync() error {
	if ew.f == nil {
		return os.ErrClosed
	}

	return ew.f.Sync()
}

//

type zeroReader struct{}

var _ io.Reader = (*zeroReader)(nil)

func (zr *zeroReader) Read(p []byte) (int, error) {
	// p can be *anything* - it can be preallocated and
	// already used in previous I/O operations. So we
	// really do need to clear it.

	// that code seems slow, but luckily it's optimized:
	// https://github.com/golang/go/wiki/CompilerOptimizations#optimized-memclr
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
