package memcache

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestTryDiscardLines(t *testing.T) {
	const item = "VALUE foo 0 3 1\r\nbar\r\n"

	tests := []struct {
		name     string
		response string
		wantErr  error
	}{
		{name: "no items", response: "END\r\n"},
		{name: "one item", response: item + "END\r\n"},
		{name: "several items", response: item + item + "END\r\n"},

		// A response that stops before the END line is what the client reads
		// when the server closes the connection.
		{name: "closed before any line", response: "", wantErr: io.ErrUnexpectedEOF},
		{name: "closed in the middle of a VALUE line", response: "VALUE foo 0", wantErr: io.ErrUnexpectedEOF},
		{name: "closed after a VALUE line", response: "VALUE foo 0 3 1\r\n", wantErr: io.ErrUnexpectedEOF},
		{name: "closed in the middle of an item", response: "VALUE foo 0 3 1\r\nba", wantErr: io.ErrUnexpectedEOF},
		{name: "closed after an item", response: item, wantErr: io.ErrUnexpectedEOF},
		{name: "closed in the middle of the END line", response: item + "EN", wantErr: io.ErrUnexpectedEOF},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tryDiscardLines(bufio.NewReader(strings.NewReader(tc.response)))
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("tryDiscardLines: got err=%v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("tryDiscardLines: got err=%v, want %v", err, tc.wantErr)
			}
		})
	}
}

func BenchmarkReadLine(b *testing.B) {
	alloc := newTestAllocator(1024 + 2) // extra 2 is for body's trailing "\r\n"

	for _, size := range []int{0, 1024} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			lineReader := allocatingLineReader{
				allocator: alloc,
			}

			resp := strings.NewReader(fmt.Sprintf("VALUE foobar 0 %v\r\n%s\r\nEND\r\n", size, strings.Repeat("a", size)))

			b.ReportAllocs()
			b.ResetTimer()

			var buf bufio.Reader
			for i := 0; i < b.N; i++ {
				_, err := resp.Seek(0, io.SeekStart)
				if err != nil {
					b.Fatal(err)
				}
				buf.Reset(resp)

				it, err := readLine(&buf, lineReader)
				if err != nil {
					b.Fatal(err)
				}
				if it.Value == nil {
					b.Fatal("unexpected nil value")
				}
				if len(it.Value) != size {
					b.Fatalf("unexpected value len: want %d, got %d bytes", size, len(it.Value))
				}

				// Note, the current option's promise is that the Client will only call Put in the event of an error.
				// That is, the callers *may* expect that they are allowed to Put the item's Value back into the pool.
				alloc.Put(&it.Value)
			}
		})
	}
}
