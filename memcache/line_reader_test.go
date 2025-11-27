package memcache

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"testing"
)

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
