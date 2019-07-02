package zipextractor_test

import (
	"bytes"
	"log"
	"testing"

	"github.com/itchio/headway/united"
	"github.com/itchio/savior"
	"github.com/itchio/savior/checker"
	"github.com/itchio/savior/zipextractor"
	"github.com/stretchr/testify/assert"
)

func must(t *testing.T, err error) {
	assert.NoError(t, err)
	if err != nil {
		t.FailNow()
	}
}

func TestZip(t *testing.T) {
	sink := checker.MakeTestSinkAdvanced(40)

	log.Printf("Making zip from checker.Sink...")
	zipBytes := checker.MakeZip(t, sink)

	makeZipExtractor := func() savior.Extractor {
		ex, err := zipextractor.New(bytes.NewReader(zipBytes), int64(len(zipBytes)))
		must(t, err)
		return ex
	}

	log.Printf("Testing .zip (%s), no resumes", united.FormatBytes(int64(len(zipBytes))))
	checker.RunExtractorText(t, makeZipExtractor, sink, func() bool {
		return false
	})

	log.Printf("Testing .zip (%s), every resume", united.FormatBytes(int64(len(zipBytes))))
	checker.RunExtractorText(t, makeZipExtractor, sink, func() bool {
		return true
	})

	log.Printf("Testing .zip (%s), every other resume", united.FormatBytes(int64(len(zipBytes))))
	i := 0
	checker.RunExtractorText(t, makeZipExtractor, sink, func() bool {
		i++
		return i%2 == 0
	})
}
