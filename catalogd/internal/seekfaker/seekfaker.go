package seekfaker

import (
	"fmt"
	"io"
)

type seekFaker struct {
	reader io.Reader // io.MultiReader
	size   int64
}

// Content-Type must ALWAYS be set before using this, because http.ServeContent will otherwise
// try a sniff test & rewind to origin, and our multireader cannot be rewound

func NewSeekFaker(size int64, reader ...io.Reader) io.ReadSeeker {
	return &seekFaker{
		reader: io.MultiReader(reader...),
		size:   size,
	}
}

func (sf *seekFaker) Read(p []byte) (n int, err error) {
	return sf.reader.Read(p)
}

func (sf *seekFaker) Seek(offset int64, whence int) (int64, error) {
	size := int64(0)
	err := error(nil)

	switch whence {
	case io.SeekStart:
		// do nothing
	case io.SeekEnd:
		size = sf.size
	default:
		err = fmt.Errorf("seek: invalid whence")
	}

	return size, err
}
