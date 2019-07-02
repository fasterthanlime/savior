package flatesource_test

import (
	"log"
	"testing"

	"github.com/itchio/headway/united"
	"github.com/itchio/savior"
	"github.com/itchio/savior/checker"
	"github.com/itchio/savior/flatesource"
	"github.com/itchio/savior/seeksource"
	"github.com/itchio/savior/semirandom"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
)

func Test_Uninitialized(t *testing.T) {
	{
		ss := seeksource.FromBytes(nil)
		_, err := ss.Resume(nil)
		assert.NoError(t, err)

		fs := flatesource.New(ss)
		_, err = fs.Read([]byte{})
		assert.Error(t, err)
		assert.True(t, errors.Cause(err) == savior.ErrUninitializedSource)

		_, err = fs.ReadByte()
		assert.Error(t, err)
		assert.True(t, errors.Cause(err) == savior.ErrUninitializedSource)
	}
}

func Test_Checkpoints(t *testing.T) {
	reference := semirandom.Bytes(4 * 1024 * 1024 /* 4 MiB of random data */)
	compressed, err := checker.FlateCompress(reference)
	assert.NoError(t, err)

	log.Printf("uncompressed size: %s", united.FormatBytes(int64(len(reference))))
	log.Printf("  compressed size: %s", united.FormatBytes(int64(len(compressed))))

	source := seeksource.FromBytes(compressed)
	fs := flatesource.New(source)

	checker.RunSourceTest(t, fs, reference)
}
