package memcache

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

type lineReader interface {
	ReadLine(from io.Reader, lineLength int) ([]byte, error)
}

type allocatingLineReader struct {
	allocator Allocator
}

func (s allocatingLineReader) ReadLine(from io.Reader, lineLength int) ([]byte, error) {
	// Get can return a larger buffer than requested, but never smaller.
	// So we need to save how much of it we actually use.
	// Expect the line to end with \r\n, so read 2 extra bytes.
	readSize := lineLength + 2
	buff := s.allocator.Get(readSize)

	destBuf := (*buff)[:readSize]
	_, err := io.ReadFull(from, destBuf)
	if err != nil {
		s.allocator.Put(buff)
		return nil, fmt.Errorf("failed to read line: %w", err)
	}
	if !bytes.HasSuffix(destBuf, crlf) {
		s.allocator.Put(buff)
		return nil, fmt.Errorf("line is not followed by CRLF")
	}
	return destBuf[:lineLength], nil
}

type noopLineReader struct{}

func (s noopLineReader) ReadLine(from io.Reader, lineLength int) ([]byte, error) {
	_, err := io.CopyN(io.Discard, from, int64(lineLength))
	if err != nil {
		return nil, fmt.Errorf("discarding line: %w", err)
	}
	return nil, nil
}

func tryDiscardLines(r *bufio.Reader) error {
	for {
		_, err := readLine(r, noopLineReader{})
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("memcache GetMulti: discarding cancelled response: %w", err)
		}
	}
	return nil
}

func readLine[R lineReader](r *bufio.Reader, buff R) (*Item, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	if bytes.Equal(line, resultEnd) {
		return nil, io.EOF
	}
	it := new(Item)
	size, err := scanGetResponseLine(line, it)
	if err != nil {
		return nil, err
	}
	it.Value, err = buff.ReadLine(r, size)
	if err != nil {
		return nil, fmt.Errorf("memcache: corrupt get result: %w", err)
	}
	return it, nil
}
