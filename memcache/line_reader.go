package memcache

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// errEndOfResponse is returned by readLine for the END line, which terminates
// a get response. It is deliberately not io.EOF: io.EOF is what the reader
// returns when the server closes the connection, and that is not the end of a
// response.
var errEndOfResponse = errors.New("memcache: end of get response")

// unexpectedEOF converts io.EOF into io.ErrUnexpectedEOF. A get response
// always ends with an END line, so a connection that is closed anywhere before
// it leaves the response incomplete.
func unexpectedEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

type lineReader interface {
	ReadLine(from io.Reader, lineLength int) ([]byte, error)
}

type allocatingLineReader struct {
	allocator Allocator
}

func (s allocatingLineReader) ReadLine(from io.Reader, lineLength int) ([]byte, error) {
	// Note that lineLength MUST account for the trailing \r\n.
	if lineLength < len(crlf) {
		return nil, errors.New("line length too small: must include CRLF")
	}

	// Get can return a larger buffer than requested, but never smaller.
	buff := s.allocator.Get(lineLength)

	destBuf := (*buff)[:lineLength]
	_, err := io.ReadFull(from, destBuf)
	if err != nil {
		s.allocator.Put(buff)
		return nil, fmt.Errorf("failed to read line: %w", unexpectedEOF(err))
	}
	if !bytes.HasSuffix(destBuf, crlf) {
		s.allocator.Put(buff)
		return nil, fmt.Errorf("line is not followed by CRLF")
	}
	return destBuf[:lineLength-len(crlf)], nil
}

type noopLineReader struct{}

func (s noopLineReader) ReadLine(from io.Reader, lineLength int) ([]byte, error) {
	_, err := io.CopyN(io.Discard, from, int64(lineLength))
	if err != nil {
		return nil, fmt.Errorf("discarding line: %w", unexpectedEOF(err))
	}
	return nil, nil
}

func tryDiscardLines(r *bufio.Reader) error {
	for {
		_, err := readLine(r, noopLineReader{})
		if errors.Is(err, errEndOfResponse) {
			return nil
		} else if err != nil {
			return fmt.Errorf("memcache GetMulti: discarding cancelled response: %w", err)
		}
	}
}

// readLine reads the next item of a get response from r. It returns
// errEndOfResponse once it reads the END line, and an error matching
// io.ErrUnexpectedEOF if the connection is closed before that.
func readLine[R lineReader](r *bufio.Reader, buff R) (*Item, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return nil, unexpectedEOF(err)
	}
	if bytes.Equal(line, resultEnd) {
		return nil, errEndOfResponse
	}
	it := new(Item)
	size, err := scanGetResponseLine(line, it)
	if err != nil {
		// The line is not a VALUE line: check whether it's a protocol-level
		// server error before reporting it as unexpected.
		if serverErr := serverErrorFromLine(line); serverErr != nil {
			return nil, serverErr
		}
		return nil, err
	}

	// Expect the line to end with \r\n
	readSize := size + len(crlf)

	it.Value, err = buff.ReadLine(r, readSize)
	if err != nil {
		return nil, fmt.Errorf("memcache: corrupt get result: %w", err)
	}
	return it, nil
}
