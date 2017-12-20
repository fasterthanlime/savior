package zipextractor

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	humanize "github.com/dustin/go-humanize"
	"github.com/itchio/savior/flatesource"
	"github.com/itchio/savior/seeksource"

	"github.com/go-errors/errors"
	"github.com/itchio/arkive/zip"
	"github.com/itchio/savior"
)

const defaultFlateThreshold = 1 * 1024 * 1024

type ZipExtractor struct {
	source savior.Source
	sink   savior.Sink

	reader     io.ReaderAt
	readerSize int64
	zr         *zip.Reader

	sc savior.SaveConsumer

	flateThreshold int64
}

var _ savior.Extractor = (*ZipExtractor)(nil)

func New(reader io.ReaderAt, readerSize int64, sink savior.Sink) *ZipExtractor {
	return &ZipExtractor{
		reader:     reader,
		readerSize: readerSize,
		sink:       sink,
		sc:         savior.NopSaveConsumer(),
	}
}

func (ze *ZipExtractor) SetSaveConsumer(sc savior.SaveConsumer) {
	ze.sc = sc
}

func (ze *ZipExtractor) SetFlateThreshold(flateThreshold int64) {
	ze.flateThreshold = flateThreshold
}

func (ze *ZipExtractor) FlateThreshold() int64 {
	if ze.flateThreshold > 0 {
		return ze.flateThreshold
	}
	return defaultFlateThreshold
}

func (ze *ZipExtractor) Resume(checkpoint *savior.ExtractorCheckpoint) error {
	var err error
	ze.zr, err = zip.NewReader(ze.reader, ze.readerSize)
	if err != nil {
		return errors.Wrap(err, 0)
	}

	if checkpoint == nil {
		checkpoint = &savior.ExtractorCheckpoint{
			EntryIndex: 0,
		}
	}

	stop := false
	numEntries := int64(len(ze.zr.File))

	for entryIndex := checkpoint.EntryIndex; entryIndex < numEntries; entryIndex++ {
		if stop {
			return savior.StopErr
		}

		savior.Debugf(`doing entryIndex %d`, entryIndex)
		zf := ze.zr.File[entryIndex]

		err := func() error {
			checkpoint.EntryIndex = entryIndex

			if checkpoint.Entry == nil {
				savior.Debugf("making fresh savior.Entry for index %d", entryIndex)
				entry := &savior.Entry{
					CanonicalPath:    filepath.ToSlash(zf.Name),
					CompressedSize:   int64(zf.CompressedSize64),
					UncompressedSize: int64(zf.UncompressedSize64),
					Mode:             zf.Mode(),
				}

				info := zf.FileInfo()

				if info.IsDir() {
					entry.Kind = savior.EntryKindDir
				} else if entry.Mode&os.ModeSymlink > 0 {
					entry.Kind = savior.EntryKindSymlink
				} else {
					entry.Kind = savior.EntryKindFile
				}
				checkpoint.Entry = entry
			}
			entry := checkpoint.Entry

			switch entry.Kind {
			case savior.EntryKindDir:
				err := ze.sink.Mkdir(entry)
				if err != nil {
					return errors.Wrap(err, 0)
				}
			case savior.EntryKindSymlink:
				rc, err := zf.Open()
				if err != nil {
					return errors.Wrap(err, 0)
				}

				defer rc.Close()

				linkname, err := ioutil.ReadAll(rc)
				if err != nil {
					return errors.Wrap(err, 0)
				}

				err = ze.sink.Symlink(entry, string(linkname))
				if err != nil {
					return errors.Wrap(err, 0)
				}
			case savior.EntryKindFile:
				var src savior.Source

				switch zf.Method {
				case zip.Store, zip.Deflate:
					dataOff, err := zf.DataOffset()
					if err != nil {
						return errors.Wrap(err, 0)
					}

					compressedSize := int64(zf.CompressedSize64)

					reader := io.NewSectionReader(ze.reader, dataOff, compressedSize)
					rawSource := seeksource.NewWithSize(reader, compressedSize)

					switch zf.Method {
					case zip.Store:
						src = rawSource
					case zip.Deflate:
						src = flatesource.New(rawSource, ze.FlateThreshold())
					}
				default:
					// will have to copy
				}

				if src == nil {
					// save/resume not supported for this storage format
					// (probably LZMA), doing a simple copy
					entry.WriteOffset = 0

					rc, err := zf.Open()
					if err != nil {
						return errors.Wrap(err, 0)
					}

					defer rc.Close()

					writer, err := ze.sink.GetWriter(entry)
					if err != nil {
						return errors.Wrap(err, 0)
					}

					_, err = io.Copy(writer, rc)
					if err != nil {
						return errors.Wrap(err, 0)
					}
				} else {
					offset, err := src.Resume(checkpoint.SourceCheckpoint)
					if err != nil {
						return errors.Wrap(err, 0)
					}

					if offset < entry.WriteOffset {
						delta := entry.WriteOffset - offset
						savior.Debugf(`%s: discarding %d bytes to align source and writer`, entry.CanonicalPath, delta)
						savior.Debugf(`%s: (source resumed at %d, writer was at %d)`, entry.CanonicalPath, offset, entry.WriteOffset)
						err := savior.DiscardByRead(src, delta)
						if err != nil {
							return errors.Wrap(err, 0)
						}
					}
					savior.Debugf(`%s: zipextractor resuming from %s`, entry.CanonicalPath, humanize.IBytes(uint64(entry.WriteOffset)))

					writer, err := ze.sink.GetWriter(entry)
					if err != nil {
						return errors.Wrap(err, 0)
					}

					copyRes, err := savior.CopyWithSaver(&savior.CopyParams{
						Src:   src,
						Dst:   writer,
						Entry: entry,

						SaveConsumer: ze.sc,
						MakeCheckpoint: func() (*savior.ExtractorCheckpoint, error) {
							sourceCheckpoint, err := src.Save()
							if err != nil {
								return nil, errors.Wrap(err, 0)
							}

							savior.Debugf(`%s: saving, has source checkpoint? %v`, entry.CanonicalPath, sourceCheckpoint != nil)
							if sourceCheckpoint != nil {
								savior.Debugf(`%s: source checkpoint is at %d`, entry.CanonicalPath, sourceCheckpoint.Offset)
							}
							checkpoint.SourceCheckpoint = sourceCheckpoint

							err = writer.Sync()
							if err != nil {
								return nil, errors.Wrap(err, 0)
							}

							return checkpoint, nil
						},
					})
					if err != nil {
						return errors.Wrap(err, 0)
					}

					if copyRes.Action == savior.AfterSaveStop {
						stop = true
						return nil
					}
				}
			}

			return nil
		}()
		if err != nil {
			return errors.Wrap(err, 0)
		}

		checkpoint.SourceCheckpoint = nil
		checkpoint.Entry = nil
	}

	return nil
}