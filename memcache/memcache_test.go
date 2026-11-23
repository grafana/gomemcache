/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package memcache provides a client for the memcached cache server.
package memcache

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func startServer(t testing.TB, host string, port int) {
	cmd := exec.Command("memcached", "-p", strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatalf("unable to start memcached server on port %d: %s", port, err)
	}

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	dialAddr := net.JoinHostPort(host, strconv.Itoa(port))
	waitForServer(t, dialAddr, 5*time.Second)
	prepareServer(t, dialAddr)
}

func waitForServer(t testing.TB, dialAddr string, timeout time.Duration) {
	const step = 100 * time.Millisecond
	waited := time.Duration(0)

	for waited < timeout {
		c, err := net.Dial("tcp", dialAddr)
		if err == nil {
			_ = c.Close()
			return
		}

		time.Sleep(step)
		waited += step
	}

	t.Fatalf("gave up waiting for memcached after %s", timeout)
}

func prepareServer(t testing.TB, dialAddr string) {
	c, err := net.Dial("tcp", dialAddr)
	if err != nil {
		t.Fatalf("unable to connect to memcached %s to prepare: %s", dialAddr, err)
	}
	_, _ = c.Write([]byte("flush_all\r\n"))
	_ = c.Close()
}

func TestLocalhost(t *testing.T) {
	startServer(t, "localhost", 11221)

	c := New(net.JoinHostPort("localhost", strconv.Itoa(11221)))
	t.Cleanup(c.Close)

	testWithClient(t, c)
}

func TestStripedServers(t *testing.T) {
	startServer(t, "localhost", 11221)
	startServer(t, "localhost", 11231)

	dialAddr1 := net.JoinHostPort("localhost", strconv.Itoa(11221))
	dialAddr2 := net.JoinHostPort("localhost", strconv.Itoa(11231))

	servers := &ServerList{}
	if err := servers.SetServers(dialAddr1, dialAddr2); err != nil {
		t.Fatalf("unable to set servers in selector: %s", err)
	}

	c := NewStripedFromSelector(servers, 2)
	t.Cleanup(c.Close)

	testWithClient(t, c)
}

// Run the memcached binary as a child process and connect to its unix socket.
func TestUnixSocket(t *testing.T) {
	sock := path.Join(t.TempDir(), fmt.Sprintf("test-gomemcache-%d.sock", os.Getpid()))
	cmd := exec.Command("memcached", "-s", sock)
	if err := cmd.Start(); err != nil {
		t.Fatalf("unable to start memcached server over unix socket %s: %s", sock, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Wait a bit for the socket to appear.
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(time.Duration(25*i) * time.Millisecond)
	}

	c := New(sock)
	t.Cleanup(c.Close)

	testWithClient(t, c)
}

func mustSetF(t *testing.T, c *Client) func(*Item) {
	return func(it *Item) {
		if err := c.Set(it); err != nil {
			t.Fatalf("failed to Set %#v: %v", *it, err)
		}
	}
}

func testWithClient(t *testing.T, c *Client) {
	checkErr := func(err error, format string, args ...interface{}) {
		if err != nil {
			t.Fatalf(format, args...)
		}
	}
	mustSet := mustSetF(t, c)

	t.Run("get and set", func(t *testing.T) {
		foo := &Item{Key: "foo", Value: []byte("fooval"), Flags: 123}
		err := c.Set(foo)
		checkErr(err, "first set(foo): %v", err)
		err = c.Set(foo)
		checkErr(err, "second set(foo): %v", err)

		it, err := c.Get("foo")
		checkErr(err, "get(foo): %v", err)
		if it.Key != "foo" {
			t.Errorf("get(foo) Key = %q, want foo", it.Key)
		}
		if string(it.Value) != "fooval" {
			t.Errorf("get(foo) Value = %q, want fooval", string(it.Value))
		}
		if it.Flags != 123 {
			t.Errorf("get(foo) Flags = %v, want 123", it.Flags)
		}
	})

	t.Run("get and set unicode key", func(t *testing.T) {
		quxKey := "Hello_世界"
		qux := &Item{Key: quxKey, Value: []byte("hello world")}
		err := c.Set(qux)
		checkErr(err, "first set(Hello_世界): %v", err)
		it, err := c.Get(quxKey)
		checkErr(err, "get(Hello_世界): %v", err)
		if it.Key != quxKey {
			t.Errorf("get(Hello_世界) Key = %q, want Hello_世界", it.Key)
		}
		if string(it.Value) != "hello world" {
			t.Errorf("get(Hello_世界) Value = %q, want hello world", string(it.Value))
		}
	})

	t.Run("get and set zero-length item", func(t *testing.T) {
		foo := &Item{Key: "zerofoo"} // memcached allows values of zero length
		err := c.Set(foo)
		checkErr(err, "first set(zerofoo): %v", err)
		err = c.Set(foo)
		checkErr(err, "second set(zerofoo): %v", err)

		it, err := c.Get("zerofoo")
		checkErr(err, "get(zerofoo): %v", err)
		if it.Key != "zerofoo" {
			t.Errorf("get(zerofoo) Key = %q, want zerofoo", it.Key)
		}
		if string(it.Value) != "" {
			t.Errorf("get(zerofoo) Value = %q, want empty", string(it.Value))
		}
		if it.Flags != 0 {
			t.Errorf("get(zerofoo) Flags = %v, want 0", it.Flags)
		}
	})

	t.Run("set malformed keys", func(t *testing.T) {
		malFormed := &Item{Key: "foo bar", Value: []byte("foobarval")}
		err := c.Set(malFormed)
		if err != ErrMalformedKey {
			t.Errorf("set(foo bar) should return ErrMalformedKey instead of %v", err)
		}
		malFormed = &Item{Key: "foo" + string(rune(0x7f)), Value: []byte("foobarval")}
		err = c.Set(malFormed)
		if err != ErrMalformedKey {
			t.Errorf("set(foo<0x7f>) should return ErrMalformedKey instead of %v", err)
		}
	})

	t.Run("add", func(t *testing.T) {
		bar := &Item{Key: "bar", Value: []byte("barval")}
		err := c.Add(bar)
		checkErr(err, "first add(foo): %v", err)
		if err := c.Add(bar); err != ErrNotStored {
			t.Fatalf("second add(foo) want ErrNotStored, got %v", err)
		}
	})

	t.Run("replace", func(t *testing.T) {
		baz := &Item{Key: "baz", Value: []byte("bazvalue")}
		if err := c.Replace(baz); err != ErrNotStored {
			t.Fatalf("expected replace(baz) to return ErrNotStored, got %v", err)
		}

		bar := &Item{Key: "bar", Value: []byte("barval")}
		err := c.Replace(bar)
		checkErr(err, "replaced(foo): %v", err)
	})

	t.Run("getmulti", func(t *testing.T) {
		m, err := c.GetMulti(context.Background(), []string{"foo", "bar"})
		checkErr(err, "GetMulti: %v", err)
		if g, e := len(m), 2; g != e {
			t.Errorf("GetMulti: got len(map) = %d, want = %d", g, e)
		}
		if _, ok := m["foo"]; !ok {
			t.Fatalf("GetMulti: didn't get key 'foo'")
		}
		if _, ok := m["bar"]; !ok {
			t.Fatalf("GetMulti: didn't get key 'bar'")
		}
		if g, e := string(m["foo"].Value), "fooval"; g != e {
			t.Errorf("GetMulti: foo: got %q, want %q", g, e)
		}
		if g, e := string(m["bar"].Value), "barval"; g != e {
			t.Errorf("GetMulti: bar: got %q, want %q", g, e)
		}
	})

	t.Run("getmulti context cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		m, err := c.GetMulti(ctx, []string{"foo", "bar"})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("GetMulti with cancelled context: got err=%v, want wrapped context.Canceled", err)
		}
		if m != nil {
			t.Errorf("GetMulti with cancelled context: got map=%v, want=nil", m)
		}
	})

	t.Run("getmulti doesn't allocate after context cancellation", func(t *testing.T) {
		mustSet(&Item{Key: "large1", Value: make([]byte, 1000)})

		// Fetch more keys to increase the chance of the cancellation happening during fetching.
		requestedKeys := slices.Repeat([]string{"large1"}, 1000)

		var avgRequestDuration time.Duration
		{
			// Run the query a few times to choose a realistic timeout
			startTime := time.Now()
			for i := 0; i < 5; i++ {
				_, _ = c.GetMulti(context.Background(), requestedKeys)
			}
			avgRequestDuration = time.Since(startTime) / 5
		}

		requestTimeout := avgRequestDuration * 2
		t.Logf("Average GetMulti duration %s; choosing max timeout of %s", avgRequestDuration, requestTimeout)

		for i := 0; i < 100; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(rand.Int63n(int64(requestTimeout))))
			t.Cleanup(cancel)
			allowedAllocations := &atomic.Bool{}
			allowedAllocations.Store(true)

			allocator := &mockAllocator{get: func(sz int) *[]byte {
				time.Sleep(10 * time.Microsecond) // Slow down allocation to make the
				if !allowedAllocations.Load() {
					t.Errorf("GetMulti allocated memory after context was cancelled")
				}
				s := make([]byte, sz)
				return &s
			}}

			start := time.Now()
			_, _ = c.GetMulti(ctx, requestedKeys, WithAllocator(allocator))
			allowedAllocations.Store(false)
			t.Logf("GetMulti took %v (cancelled: %t)", time.Since(start), ctx.Err() != nil)
		}
	})

	t.Run("getmulti context cancellation during response parsing", func(t *testing.T) {
		// Set up items with some data
		mustSet(&Item{Key: "large1", Value: make([]byte, 1000)})
		mustSet(&Item{Key: "large2", Value: make([]byte, 1000)})

		ctx, cancel := context.WithCancel(context.Background())

		// Start GetMulti in a goroutine
		resultCh := make(chan map[string]*Item, 1)
		errCh := make(chan error, 1)
		go func() {
			m, err := c.GetMulti(ctx, []string{"large1", "large2"})
			resultCh <- m
			errCh <- err
		}()

		// Cancel context immediately to trigger cancellation during parsing
		cancel()

		// Should get cancelled context error
		err := <-errCh
		m := <-resultCh

		if !errors.Is(err, context.Canceled) {
			t.Errorf("GetMulti with cancelled context during parsing: got err=%v, want wrapped context.Canceled", err)
		}
		if m != nil {
			t.Errorf("GetMulti with cancelled context during parsing: got map=%v, want=nil", m)
		}

		// Connection should still be usable - test with a simple get
		_, err = c.Get("foo")
		if err != nil && err != ErrCacheMiss {
			t.Errorf("Connection should be usable after cancelled GetMulti: got err=%v", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		err := c.Delete("foo")
		checkErr(err, "Delete: %v", err)
		_, err = c.Get("foo")
		if err != ErrCacheMiss {
			t.Errorf("post-Delete want ErrCacheMiss, got %v", err)
		}
	})

	t.Run("incr decr", func(t *testing.T) {
		mustSet(&Item{Key: "num", Value: []byte("42")})
		n, err := c.Increment("num", 8)
		checkErr(err, "Increment num + 8: %v", err)
		if n != 50 {
			t.Fatalf("Increment num + 8: want=50, got=%d", n)
		}
		n, err = c.Decrement("num", 49)
		checkErr(err, "Decrement: %v", err)
		if n != 1 {
			t.Fatalf("Decrement 49: want=1, got=%d", n)
		}
		err = c.Delete("num")
		checkErr(err, "delete num: %v", err)
		_, err = c.Increment("num", 1)
		if err != ErrCacheMiss {
			t.Fatalf("increment post-delete: want ErrCacheMiss, got %v", err)
		}
		mustSet(&Item{Key: "num", Value: []byte("not-numeric")})
		_, err = c.Increment("num", 1)
		if err == nil || !strings.Contains(err.Error(), "client error") {
			t.Fatalf("increment non-number: want client error, got %v", err)
		}
	})

	t.Run("delete all", func(t *testing.T) {
		err := c.DeleteAll()
		checkErr(err, "DeleteAll: %v", err)
		_, err = c.Get("bar")
		if err != ErrCacheMiss {
			t.Errorf("post-DeleteAll want ErrCacheMiss, got %v", err)
		}
	})

	t.Run("ping", func(t *testing.T) {
		err := c.Ping()
		checkErr(err, "error ping: %s", err)
	})

	t.Run("touch", func(t *testing.T) {
		testTouchWithClient(t, c)
	})

	t.Run("compare and swap", func(t *testing.T) {
		mustSet(&Item{Key: "cas", Value: []byte("v1")})
		it, err := c.Get("cas")
		checkErr(err, "get(cas): %v", err)
		if it.CasID == 0 {
			t.Fatal("get(cas) CasID = 0, want the token returned by gets")
		}

		// A CAS built from the token stores; it may change any field but Key.
		fresh := &Item{Key: "cas", Value: []byte("v2"), Expiration: 60, CasID: it.CasID}
		err = c.CompareAndSwap(fresh)
		checkErr(err, "cas with current token: %v", err)

		// That store bumped the token, so the same token now conflicts.
		if err := c.CompareAndSwap(fresh); err != ErrCASConflict {
			t.Fatalf("cas with stale token want ErrCASConflict, got %v", err)
		}
		// An item re-created under the same key has a new token too.
		checkErr(c.Delete("cas"), "delete(cas)")
		checkErr(c.Add(&Item{Key: "cas", Value: []byte("v3")}), "add(cas)")
		if err := c.CompareAndSwap(fresh); err != ErrCASConflict {
			t.Fatalf("cas after re-add want ErrCASConflict, got %v", err)
		}
		checkErr(c.Delete("cas"), "delete(cas)")
		if err := c.CompareAndSwap(fresh); err != ErrCacheMiss {
			t.Fatalf("cas on a missing key want ErrCacheMiss, got %v", err)
		}
	})

	t.Run("get with allocator", func(t *testing.T) {
		foo := &Item{Key: "foo", Value: []byte("fooval"), Flags: 123}
		err := c.Set(foo)
		checkErr(err, "first set(foo): %v", err)

		alloc := newTestAllocator(len(foo.Value) + 2)
		it, err := c.Get("foo", WithAllocator(alloc))
		checkErr(err, "get(foo): %v", err)
		t.Cleanup(func() {
			alloc.Put(&it.Value)
		})

		if it.Key != "foo" {
			t.Errorf("get(foo) Key = %q, want foo", it.Key)
		}
		if string(it.Value) != "fooval" {
			t.Errorf("get(foo) Value = %q, want fooval", string(it.Value))
		}
		if alloc.numGets != 1 {
			t.Errorf("get(foo) num gets from Allocator = %d, want 1", alloc.numGets)
		}
	})
}

func testTouchWithClient(t *testing.T, c *Client) {
	mustSet := mustSetF(t, c)

	const secondsToExpiry = int32(2)

	// We will set foo and bar to expire in 2 seconds, then we'll keep touching
	// foo every second
	// After 3 seconds, we expect foo to be available, and bar to be expired
	foo := &Item{Key: "foo", Value: []byte("fooval"), Expiration: secondsToExpiry}
	bar := &Item{Key: "bar", Value: []byte("barval"), Expiration: secondsToExpiry}

	setTime := time.Now()
	mustSet(foo)
	mustSet(bar)

	for s := 0; s < 3; s++ {
		time.Sleep(1 * time.Second)
		err := c.Touch(foo.Key, secondsToExpiry)
		if nil != err {
			t.Errorf("error touching foo: %v", err.Error())
		}
	}

	_, err := c.Get("foo")
	if err != nil {
		if err == ErrCacheMiss {
			t.Fatalf("touching failed to keep item foo alive")
		} else {
			t.Fatalf("unexpected error retrieving foo after touching: %v", err.Error())
		}
	}

	_, err = c.Get("bar")
	if nil == err {
		t.Fatalf("item bar did not expire within %v seconds", time.Since(setTime).Seconds())
	} else {
		if err != ErrCacheMiss {
			t.Fatalf("unexpected error retrieving bar: %v", err.Error())
		}
	}
}

func TestClient_releaseIdleConnections(t *testing.T) {
	const recentlyUsedThreshold = 2 * time.Second

	startServer(t, "localhost", 11221)

	getClientWithMinIdleConnsHeadroomPercentage := func(t *testing.T, headroomPercentage float64) *Client {
		c := New(net.JoinHostPort("localhost", strconv.Itoa(11221)))
		t.Cleanup(c.Close)
		c.MinIdleConnsHeadroomPercentage = headroomPercentage
		c.MaxIdleConns = 100
		c.RecentlyUsedConnsThreshold = recentlyUsedThreshold

		return c
	}

	getConn := func(c *Client) *conn {
		addr, err := c.selector.PickServer("test")
		if err != nil {
			t.Fatalf("unexpected error picking server: %v", err.Error())
		}

		connection, err := c.getConn(addr)
		if err != nil {
			t.Fatalf("unexpected error getting connection: %v", err.Error())
		}

		return connection
	}

	countFreeConns := func(c *Client) (recentlyUsed, idle int) {
		for _, stripe := range c.stripes {
			stripe.mtx.Lock()

			for _, freeConnections := range stripe.free {
				for _, freeConn := range freeConnections {
					if time.Since(freeConn.idleSince) >= c.RecentlyUsedConnsThreshold {
						idle++
					} else {
						recentlyUsed++
					}
				}
			}

			stripe.mtx.Unlock()
		}

		return
	}

	t.Run("noop if there are no free connections", func(t *testing.T) {
		c := getClientWithMinIdleConnsHeadroomPercentage(t, 50)
		c.releaseIdleConnections()

		numRecentlyUsed, numIdle := countFreeConns(c)
		if numRecentlyUsed != 0 {
			t.Fatalf("expected %d recently used connections but got %d", 0, numRecentlyUsed)
		}
		if numIdle != 0 {
			t.Fatalf("expected %d idle connections but got %d", 0, numIdle)
		}
	})

	t.Run("should not release recently used connections", func(t *testing.T) {
		c := getClientWithMinIdleConnsHeadroomPercentage(t, 50)

		conn1 := getConn(c)
		conn2 := getConn(c)
		conn1.release()
		conn2.release()

		c.releaseIdleConnections()

		numRecentlyUsed, numIdle := countFreeConns(c)
		if numRecentlyUsed != 2 {
			t.Fatalf("expected %d recently used connections but got %d", 2, numRecentlyUsed)
		}
		if numIdle != 0 {
			t.Fatalf("expected %d idle connections but got %d", 0, numIdle)
		}
	})

	t.Run("should release idle connections while honoring the configured headroom", func(t *testing.T) {
		c := getClientWithMinIdleConnsHeadroomPercentage(t, 50)

		conn1 := getConn(c)
		conn2 := getConn(c)
		conn3 := getConn(c)
		conn4 := getConn(c)

		conn1.release()
		conn2.release()
		time.Sleep(recentlyUsedThreshold)
		conn3.release()
		conn4.release()

		c.releaseIdleConnections()

		numRecentlyUsed, numIdle := countFreeConns(c)
		if numRecentlyUsed != 2 {
			t.Fatalf("expected %d recently used connections but got %d", 2, numRecentlyUsed)
		}
		if numIdle != 1 {
			t.Fatalf("expected %d idle connections but got %d", 1, numIdle)
		}
	})

	t.Run("should release all idle connections if headroom is zero", func(t *testing.T) {
		c := getClientWithMinIdleConnsHeadroomPercentage(t, 0)

		conn1 := getConn(c)
		conn2 := getConn(c)
		conn3 := getConn(c)
		conn4 := getConn(c)

		conn1.release()
		conn2.release()
		time.Sleep(recentlyUsedThreshold)
		conn3.release()
		conn4.release()

		c.releaseIdleConnections()

		numRecentlyUsed, numIdle := countFreeConns(c)
		if numRecentlyUsed != 2 {
			t.Fatalf("expected %d recently used connections but got %d", 2, numRecentlyUsed)
		}
		if numIdle != 0 {
			t.Fatalf("expected %d idle connections but got %d", 0, numIdle)
		}
	})

	t.Run("should not release idle connections if headroom is disabled (negative value)", func(t *testing.T) {
		c := getClientWithMinIdleConnsHeadroomPercentage(t, -1)

		conn1 := getConn(c)
		conn2 := getConn(c)
		conn3 := getConn(c)
		conn4 := getConn(c)

		conn1.release()
		conn2.release()
		time.Sleep(recentlyUsedThreshold)
		conn3.release()
		conn4.release()

		c.releaseIdleConnections()

		numRecentlyUsed, numIdle := countFreeConns(c)
		if numRecentlyUsed != 2 {
			t.Fatalf("expected %d recently used connections but got %d", 2, numRecentlyUsed)
		}
		if numIdle != 2 {
			t.Fatalf("expected %d idle connections but got %d", 2, numIdle)
		}
	})

	t.Run("should not release idle connections if headroom is 100%", func(t *testing.T) {
		c := getClientWithMinIdleConnsHeadroomPercentage(t, 100)

		conn1 := getConn(c)
		conn2 := getConn(c)
		conn3 := getConn(c)
		conn4 := getConn(c)

		conn1.release()
		conn2.release()
		time.Sleep(recentlyUsedThreshold)
		conn3.release()
		conn4.release()

		c.releaseIdleConnections()

		numRecentlyUsed, numIdle := countFreeConns(c)
		if numRecentlyUsed != 2 {
			t.Fatalf("expected %d recently used connections but got %d", 2, numRecentlyUsed)
		}
		if numIdle != 2 {
			t.Fatalf("expected %d idle connections but got %d", 2, numIdle)
		}
	})

	t.Run("should allow to set an headroom percentage > 100%", func(t *testing.T) {
		c := getClientWithMinIdleConnsHeadroomPercentage(t, 200)

		conn1 := getConn(c)
		conn2 := getConn(c)
		conn3 := getConn(c)
		conn4 := getConn(c)

		conn1.release()
		conn2.release()
		conn3.release()
		time.Sleep(recentlyUsedThreshold)
		conn4.release()

		c.releaseIdleConnections()

		numRecentlyUsed, numIdle := countFreeConns(c)
		if numRecentlyUsed != 1 {
			t.Fatalf("expected %d recently used connections but got %d", 1, numRecentlyUsed)
		}
		if numIdle != 2 {
			t.Fatalf("expected %d idle connections but got %d", 2, numIdle)
		}
	})

	t.Run("should keep all idle connections open if the computed headroom is greater than the number of available free connections", func(t *testing.T) {
		c := getClientWithMinIdleConnsHeadroomPercentage(t, 1000)

		conn1 := getConn(c)
		conn2 := getConn(c)
		conn3 := getConn(c)
		conn4 := getConn(c)

		conn1.release()
		conn2.release()
		conn3.release()
		time.Sleep(recentlyUsedThreshold)
		conn4.release()

		c.releaseIdleConnections()

		numRecentlyUsed, numIdle := countFreeConns(c)
		if numRecentlyUsed != 1 {
			t.Fatalf("expected %d recently used connections but got %d", 1, numRecentlyUsed)
		}
		if numIdle != 3 {
			t.Fatalf("expected %d idle connections but got %d", 3, numIdle)
		}
	})
}

func TestClient_GetMulti_ContextCancelled(t *testing.T) {
	t.Run("should respect context cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		c, srvw, srvReqs := getClientWithFakeServer(t)
		errCh := make(chan error)
		go func() {
			_, err := c.GetMulti(ctx, []string{"key1", "key2"})
			errCh <- err
		}()

		req := <-srvReqs
		if string(req) != "gets key1 key2\r\n" {
			t.Fatalf("unexpected GetMulti request: got %s", string(req))
		}

		// Send only a header portion of the response, causing the client to block, waiting for the rest.
		if _, err := io.WriteString(srvw, "VALUE key1 0 5\r\n"); err != nil {
			t.Fatalf("write GetMulti response header: %v", err)
		}

		// Cancel context immediately to trigger cancellation during parsing
		cancel()

		// Send the rest of the response to unblock the client and to trigger the connection draining.
		if _, err := io.WriteString(srvw, "abcde\r\nVALUE key2 0 3\r\nabc\r\nEND\r\n"); err != nil {
			t.Fatalf("write GetMulti response: %v", err)
		}

		err := <-errCh
		if !errors.Is(err, context.Canceled) {
			t.Errorf("GetMulti with cancelled context during parsing: got err=%v, want context.Canceled", err)
		}
	})

	t.Run("should respect context cancelled when connection draining fails", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		c, srvw, srvReqs := getClientWithFakeServer(t)
		errCh := make(chan error)
		go func() {
			_, err := c.GetMulti(ctx, []string{"key1", "key2"})
			errCh <- err
		}()

		req := <-srvReqs
		if string(req) != "gets key1 key2\r\n" {
			t.Fatalf("unexpected GetMulti request: got %s", string(req))
		}

		// Send only a header portion of the response, causing the client to block, waiting for the rest.
		if _, err := io.WriteString(srvw, "VALUE key1 0 5\r\n"); err != nil {
			t.Fatalf("write GetMulti response header: %v", err)
		}

		// Cancel context immediately to trigger cancellation during parsing
		cancel()

		// This chunk of the response isn't complete, because it doesn't end with "END\r\n".
		// Draining this request will cause the connection to timeout. This shouldn't be visible to the caller, because the context was already cancelled.
		if _, err := io.WriteString(srvw, "abcde\r\nVALUE key2 0 3\r\n"); err != nil {
			t.Fatalf("write GetMulti response: %v", err)
		}

		err := <-errCh
		if !errors.Is(err, context.Canceled) {
			t.Errorf("GetMulti with cancelled context during parsing: got err=%v, want context.Canceled", err)
		}
	})
}

func TestServerError_KeepsConnectionOpen(t *testing.T) {
	// countFreeConns returns the number of connections in the client's free pool.
	countFreeConns := func(c *Client) int {
		total := 0

		for _, stripe := range c.stripes {
			stripe.mtx.Lock()
			total += len(stripe.free[dummyAddr{}.String()])
			stripe.mtx.Unlock()
		}

		return total
	}

	// assertReusable verifies that the connection went back to the free pool and
	// that a subsequent request over the very same connection still works.
	assertReusable := func(t *testing.T, c *Client, srvw io.Writer, srvReqs <-chan string) {
		t.Helper()

		if got := countFreeConns(c); got != 1 {
			t.Fatalf("free conns after SERVER_ERROR: got %d, want 1", got)
		}

		errCh := make(chan error)
		go func() {
			_, err := c.Get("foo")
			errCh <- err
		}()

		if req := <-srvReqs; req != "gets foo\r\n" {
			t.Fatalf("unexpected follow-up request: %q", req)
		}
		if _, err := io.WriteString(srvw, "END\r\n"); err != nil {
			t.Fatalf("write follow-up response: %v", err)
		}

		if err := <-errCh; !errors.Is(err, ErrCacheMiss) {
			t.Fatalf("follow-up Get on reused connection: got err=%v, want ErrCacheMiss", err)
		}
	}

	t.Run("Set", func(t *testing.T) {
		c, srvw, srvReqs := getClientWithFakeServer(t)

		errCh := make(chan error)
		go func() {
			errCh <- c.Set(&Item{Key: "foo", Value: []byte("hello")})
		}()

		if req := <-srvReqs; req != "set foo 0 0 5\r\n" {
			t.Fatalf("unexpected request: %q", req)
		}
		<-srvReqs // data block
		if _, err := io.WriteString(srvw, "SERVER_ERROR object too large for cache\r\n"); err != nil {
			t.Fatalf("write response: %v", err)
		}

		err := <-errCh
		if !errors.Is(err, ErrServerError) {
			t.Fatalf("Set: got err=%v, want ErrServerError", err)
		}
		if want := "memcache: server error: object too large for cache"; err.Error() != want {
			t.Fatalf("Set: got err=%q, want %q", err.Error(), want)
		}

		assertReusable(t, c, srvw, srvReqs)
	})

	t.Run("Get", func(t *testing.T) {
		c, srvw, srvReqs := getClientWithFakeServer(t)

		errCh := make(chan error)
		go func() {
			_, err := c.Get("foo")
			errCh <- err
		}()

		if req := <-srvReqs; req != "gets foo\r\n" {
			t.Fatalf("unexpected request: %q", req)
		}
		if _, err := io.WriteString(srvw, "SERVER_ERROR out of memory writing get response\r\n"); err != nil {
			t.Fatalf("write response: %v", err)
		}

		if err := <-errCh; !errors.Is(err, ErrServerError) {
			t.Fatalf("Get: got err=%v, want ErrServerError", err)
		}

		assertReusable(t, c, srvw, srvReqs)
	})

	t.Run("Delete", func(t *testing.T) {
		c, srvw, srvReqs := getClientWithFakeServer(t)

		errCh := make(chan error)
		go func() {
			errCh <- c.Delete("foo")
		}()

		if req := <-srvReqs; req != "delete foo\r\n" {
			t.Fatalf("unexpected request: %q", req)
		}
		if _, err := io.WriteString(srvw, "SERVER_ERROR temporary failure\r\n"); err != nil {
			t.Fatalf("write response: %v", err)
		}

		if err := <-errCh; !errors.Is(err, ErrServerError) {
			t.Fatalf("Delete: got err=%v, want ErrServerError", err)
		}

		assertReusable(t, c, srvw, srvReqs)
	})

	t.Run("Increment", func(t *testing.T) {
		c, srvw, srvReqs := getClientWithFakeServer(t)

		errCh := make(chan error)
		go func() {
			_, err := c.Increment("foo", 1)
			errCh <- err
		}()

		if req := <-srvReqs; req != "incr foo 1\r\n" {
			t.Fatalf("unexpected request: %q", req)
		}
		if _, err := io.WriteString(srvw, "SERVER_ERROR temporary failure\r\n"); err != nil {
			t.Fatalf("write response: %v", err)
		}

		if err := <-errCh; !errors.Is(err, ErrServerError) {
			t.Fatalf("Increment: got err=%v, want ErrServerError", err)
		}

		assertReusable(t, c, srvw, srvReqs)
	})

	t.Run("non-protocol errors still close the connection", func(t *testing.T) {
		c, srvw, srvReqs := getClientWithFakeServer(t)

		errCh := make(chan error)
		go func() {
			errCh <- c.Set(&Item{Key: "foo", Value: []byte("hello")})
		}()

		<-srvReqs // command line
		<-srvReqs // data block
		if _, err := io.WriteString(srvw, "BOGUS RESPONSE\r\n"); err != nil {
			t.Fatalf("write response: %v", err)
		}

		err := <-errCh
		if err == nil || errors.Is(err, ErrServerError) {
			t.Fatalf("Set: got err=%v, want a non-ErrServerError error", err)
		}
		if got := countFreeConns(c); got != 0 {
			t.Fatalf("free conns after unexpected response: got %d, want 0", got)
		}
	})
}

// getClientWithFakeServer creates a new client, whose dial function is bound to an in-memory synchronous server.
func getClientWithFakeServer(t *testing.T) (*Client, io.Writer, <-chan string) {
	cliConn, srvConn := net.Pipe()
	t.Cleanup(func() {
		_ = srvConn.Close()
		_ = cliConn.Close()
	})

	c := NewFromSelector(dummySelector{})
	c.DialTimeout = func(string, string, time.Duration) (net.Conn, error) {
		return cliConn, nil
	}

	srvReqs := make(chan string)
	go func() {
		br := bufio.NewReader(srvConn)
		for {
			// ignores error for simplicity
			line, _ := br.ReadSlice('\n')
			srvReqs <- string(line)
		}
	}()

	return c, srvConn, srvReqs
}

type dummyAddr struct{}

func (t dummyAddr) Network() string { return "dummy" }

func (t dummyAddr) String() string { return "dummy" }

type dummySelector struct{}

func (d dummySelector) PickServer(string) (net.Addr, error) {
	return dummyAddr{}, nil
}

func (d dummySelector) Each(f func(net.Addr) error) error {
	return f(dummyAddr{})
}

func TestClient_Close_ShouldBeIdempotent(t *testing.T) {
	startServer(t, "localhost", 11221)

	c := New(net.JoinHostPort("localhost", strconv.Itoa(11221)))

	// Call Close twice and make sure it doesn't panic the 2nd time.
	c.Close()
	c.Close()
}

func BenchmarkGetMulti(b *testing.B) {
	startServer(b, "localhost", 11221)
	c := New(net.JoinHostPort("localhost", strconv.Itoa(11221)))
	b.Cleanup(func() {
		c.Close()
	})

	var keys []string
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("bench-%d", i)
		keys = append(keys, key)

		err := c.Set(&Item{
			Key:        key,
			Value:      []byte(strings.Repeat("bench payload", 10)),
			Flags:      0,
			Expiration: 30,
		})
		if err != nil {
			b.Fatal("Could not set item for test server: ", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.GetMulti(context.Background(), keys)
	}
}

func BenchmarkOnItem(b *testing.B) {
	fakeServer, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		b.Fatal("Could not open fake server: ", err)
	}
	b.Cleanup(func() {
		_ = fakeServer.Close()
	})

	go func() {
		for {
			if c, err := fakeServer.Accept(); err == nil {
				go func() { _, _ = io.Copy(io.Discard, c) }()
			} else {
				return
			}
		}
	}()

	addr := fakeServer.Addr()
	c := New(addr.String())
	b.Cleanup(c.Close)

	if _, err := c.getConn(addr); err != nil {
		b.Fatal("failed to initialize connection to fake server")
	}

	item := Item{Key: "foo"}
	dummyFn := func(_ *Client, _ *bufio.ReadWriter, _ *Item) error { return nil }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.onItem(&item, "", dummyFn)
	}
}

func BenchmarkPickStripe(b *testing.B) {
	c := Client{}
	c.stripes = []*freeConnectionStripe{
		{
			mtx:  sync.Mutex{},
			free: make(map[string][]*conn),
		},
		{
			mtx:  sync.Mutex{},
			free: make(map[string][]*conn),
		},
	}

	keys := []string{
		"key-something-something-something-something-something-1",
		"key-something-something-something-something-something-2",
		"key-something-something-something-something-something-3",
		"key-something-something-something-something-something-4",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for i := range keys {
			_ = c.pickStripe(keys[i])
		}
	}
}

// Benchmark the effect of adding padding to freeConnectionStripe to make sure
// each stripe is on a different cache line (usually 64 bytes) which avoids false
// sharing.
func BenchmarkFreeConnectionStripeLayout(b *testing.B) {
	const numStripes = 16

	stripes := make([]*freeConnectionStripe, numStripes)
	for i := range numStripes {
		stripes[i] = &freeConnectionStripe{
			mtx:  sync.Mutex{},
			free: make(map[string][]*conn),
		}
	}

	var idx atomic.Int32
	b.RunParallel(func(pb *testing.PB) {
		stripe := stripes[int(idx.Add(1))%len(stripes)]
		for pb.Next() {
			// Without padding, locking the mutex of one stripe would invalidate
			// the cached value of an adjacent stripe.
			stripe.mtx.Lock()
			stripe.mtx.Unlock() //nolint:staticcheck
		}
	})
}

func BenchmarkScanGetResponseLine(b *testing.B) {
	line := []byte("VALUE foobar1234 0 4096 1234\r\n")
	var it Item
	for i := 0; i < b.N; i++ {
		_, err := scanGetResponseLine(line, &it)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseGetResponse(b *testing.B) {
	valueSize := 500
	response := strings.NewReader(fmt.Sprintf("VALUE foobar1234 0 %v 1234\r\n%s\r\nEND\r\n", valueSize, strings.Repeat("a", valueSize)))

	opts := newOptions(WithAllocator(newTestAllocator(valueSize + 2)))
	reader := bufio.NewReader(response)

	startServer(b, "localhost", 11221)
	serverAddr := net.JoinHostPort("localhost", strconv.Itoa(11221))

	c := New(serverAddr)
	b.Cleanup(func() {
		c.Close()
	})

	addr, err := net.ResolveTCPAddr("tcp", serverAddr)
	if err != nil {
		b.Fatalf("failed to resolve address %q: %q", serverAddr, err)
	}

	cn, err := c.getConn(addr)
	if err != nil {
		b.Fatalf("failed to get connection: %q", err)
	}

	for i := 0; i < b.N; i++ {
		err := c.parseGetResponse(context.Background(), reader, cn, opts, func(it *Item) {
			opts.Alloc.Put(&it.Value)
		})
		if err != nil {
			b.Fatal(err)
		}
		_, err = response.Seek(0, 0)
		if err != nil {
			b.Fatal(err)
		}
		reader.Reset(response)

	}
}

type testAllocator struct {
	pool    sync.Pool
	maxSize int
	numGets int
	numPuts int
}

func newTestAllocator(maxSize int) *testAllocator {
	return &testAllocator{
		maxSize: maxSize,
		pool: sync.Pool{
			New: func() interface{} {
				b := make([]byte, maxSize)
				return &b
			},
		},
	}
}

func (p *testAllocator) Get(sz int) *[]byte {
	if sz > p.maxSize {
		panic("unexpected allocation size in test allocator")
	}

	p.numGets += 1
	bufPtr := p.pool.Get().(*[]byte)
	return bufPtr
}

func (p *testAllocator) Put(b *[]byte) {
	p.numPuts += 1
	p.pool.Put(b)
}

type mockAllocator struct {
	get func(sz int) *[]byte
	put func(b *[]byte)
}

func (m *mockAllocator) Get(sz int) *[]byte {
	return m.get(sz)
}

func (m *mockAllocator) Put(b *[]byte) {
	m.put(b)
}

func TestWithShardKey(t *testing.T) {
	ports := []int{11241, 11251, 11261}
	addrs := make([]string, len(ports))
	direct := make([]*Client, len(ports))
	for i, port := range ports {
		startServer(t, "localhost", port)
		addrs[i] = net.JoinHostPort("localhost", strconv.Itoa(port))
		direct[i] = New(addrs[i])
		t.Cleanup(direct[i].Close)
	}

	servers := &ServerList{}
	if err := servers.SetServers(addrs...); err != nil {
		t.Fatalf("unable to set servers in selector: %s", err)
	}
	selector := &recordingSelector{ServerSelector: servers}
	c := NewFromSelector(selector)
	t.Cleanup(c.Close)

	ctx := context.Background()

	t.Run("GetMulti with the shard key finds the items that Set stored on one server", func(t *testing.T) {
		keys := storeItems(t, c, servers, "found", 20, "object-a")

		got, err := c.GetMulti(ctx, keys, WithShardKey("object-a"))
		if err != nil {
			t.Fatalf("GetMulti: %v", err)
		}
		if len(got) != len(keys) {
			t.Fatalf("GetMulti found %d of %d items", len(got), len(keys))
		}
		for _, key := range keys {
			if string(got[key].Value) != key {
				t.Fatalf("GetMulti returned value %q for key %q", got[key].Value, key)
			}
		}

		holders := 0
		for i, d := range direct {
			onServer, err := d.GetMulti(ctx, keys)
			if err != nil {
				t.Fatalf("GetMulti on server %d: %v", i, err)
			}
			switch len(onServer) {
			case len(keys):
				holders++
			case 0:
			default:
				t.Fatalf("server %d holds %d of %d items, want all or none", i, len(onServer), len(keys))
			}
		}
		if holders != 1 {
			t.Fatalf("%d servers hold the items, want 1", holders)
		}
	})

	t.Run("GetMulti without the shard key misses the items that Set stored with one", func(t *testing.T) {
		keys := storeItems(t, c, servers, "missed", 20, "object-b")

		got, err := c.GetMulti(ctx, keys)
		if err != nil {
			t.Fatalf("GetMulti: %v", err)
		}
		if len(got) >= len(keys) {
			t.Fatalf("GetMulti found %d of %d items without the shard key", len(got), len(keys))
		}
	})

	t.Run("GetMulti with the shard key asks the selector once, with the shard key", func(t *testing.T) {
		keys := storeItems(t, c, servers, "once", 5, "object-c")
		selector.reset()

		if _, err := c.GetMulti(ctx, keys, WithShardKey("object-c")); err != nil {
			t.Fatalf("GetMulti: %v", err)
		}
		if got := selector.pickedKeys(); !slices.Equal(got, []string{"object-c"}) {
			t.Fatalf("the selector picked %q, want only the shard key", got)
		}
	})

	t.Run("GetMulti without the shard key asks the selector for each key", func(t *testing.T) {
		keys := storeItems(t, c, servers, "each", 5, "")
		selector.reset()

		if _, err := c.GetMulti(ctx, keys); err != nil {
			t.Fatalf("GetMulti: %v", err)
		}
		picked := selector.pickedKeys()
		slices.Sort(picked)
		want := slices.Clone(keys)
		slices.Sort(want)
		if !slices.Equal(picked, want) {
			t.Fatalf("the selector picked %q, want %q", picked, want)
		}
	})

	t.Run("GetMulti with the shard key and no keys asks no server", func(t *testing.T) {
		selector.reset()

		got, err := c.GetMulti(ctx, nil, WithShardKey("object-d"))
		if err != nil {
			t.Fatalf("GetMulti: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("GetMulti returned %v, want an empty map", got)
		}
		if picked := selector.pickedKeys(); len(picked) != 0 {
			t.Fatalf("the selector picked %q, want nothing", picked)
		}
	})

	t.Run("GetMulti with the shard key returns ErrMalformedKey for an illegal key", func(t *testing.T) {
		selector.reset()

		_, err := c.GetMulti(ctx, []string{"legal", "not legal"}, WithShardKey("object-e"))
		if !errors.Is(err, ErrMalformedKey) {
			t.Fatalf("GetMulti returned %v, want ErrMalformedKey", err)
		}
		if picked := selector.pickedKeys(); len(picked) != 0 {
			t.Fatalf("the selector picked %q, want nothing", picked)
		}
	})

	t.Run("Get finds an item that Set stored with the shard key", func(t *testing.T) {
		key := "get-key"
		shardKey := stringOnOtherServer(t, servers, key)
		if err := c.Set(&Item{Key: key, Value: []byte("v")}, WithShardKey(shardKey)); err != nil {
			t.Fatalf("Set: %v", err)
		}

		item, err := c.Get(key, WithShardKey(shardKey))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if string(item.Value) != "v" {
			t.Fatalf("Get returned value %q, want %q", item.Value, "v")
		}
		if _, err := c.Get(key); !errors.Is(err, ErrCacheMiss) {
			t.Fatalf("Get without the shard key returned %v, want ErrCacheMiss", err)
		}
	})

	t.Run("Add and Replace use the server of the shard key", func(t *testing.T) {
		key := "add-replace-key"
		shardKey := stringOnOtherServer(t, servers, key)
		withShardKey := WithShardKey(shardKey)

		if err := c.Replace(&Item{Key: key, Value: []byte("1")}, withShardKey); !errors.Is(err, ErrNotStored) {
			t.Fatalf("Replace of a missing item returned %v, want ErrNotStored", err)
		}
		if err := c.Add(&Item{Key: key, Value: []byte("1")}, withShardKey); err != nil {
			t.Fatalf("Add: %v", err)
		}
		if err := c.Add(&Item{Key: key, Value: []byte("2")}, withShardKey); !errors.Is(err, ErrNotStored) {
			t.Fatalf("Add of an existing item returned %v, want ErrNotStored", err)
		}
		if err := c.Replace(&Item{Key: key, Value: []byte("3")}, withShardKey); err != nil {
			t.Fatalf("Replace: %v", err)
		}

		item, err := c.Get(key, withShardKey)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if string(item.Value) != "3" {
			t.Fatalf("Get returned value %q, want %q", item.Value, "3")
		}
	})

	t.Run("CompareAndSwap swaps an item that sits on the server of the shard key", func(t *testing.T) {
		key := "cas-key"
		shardKey := stringOnOtherServer(t, servers, key)
		withShardKey := WithShardKey(shardKey)
		if err := c.Set(&Item{Key: key, Value: []byte("old")}, withShardKey); err != nil {
			t.Fatalf("Set: %v", err)
		}

		item, err := c.Get(key, withShardKey)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		item.Value = []byte("new")
		if err := c.CompareAndSwap(item, withShardKey); err != nil {
			t.Fatalf("CompareAndSwap: %v", err)
		}

		item, err = c.Get(key, withShardKey)
		if err != nil {
			t.Fatalf("Get after CompareAndSwap: %v", err)
		}
		if string(item.Value) != "new" {
			t.Fatalf("Get returned value %q, want %q", item.Value, "new")
		}
	})

	t.Run("Touch, Increment, Decrement and Delete act on an item that sits on the server of the shard key", func(t *testing.T) {
		key := "counter-key"
		shardKey := stringOnOtherServer(t, servers, key)
		withShardKey := WithShardKey(shardKey)
		if err := c.Set(&Item{Key: key, Value: []byte("10")}, withShardKey); err != nil {
			t.Fatalf("Set: %v", err)
		}

		if err := c.Touch(key, 100, withShardKey); err != nil {
			t.Fatalf("Touch: %v", err)
		}
		if got, err := c.Increment(key, 5, withShardKey); err != nil || got != 15 {
			t.Fatalf("Increment returned %d, %v, want 15, nil", got, err)
		}
		if got, err := c.Decrement(key, 3, withShardKey); err != nil || got != 12 {
			t.Fatalf("Decrement returned %d, %v, want 12, nil", got, err)
		}
		if err := c.Delete(key, withShardKey); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := c.Get(key, withShardKey); !errors.Is(err, ErrCacheMiss) {
			t.Fatalf("Get after Delete returned %v, want ErrCacheMiss", err)
		}
	})

	t.Run("GetMulti with the shard key returns the items that arrived before a server error", func(t *testing.T) {
		type result struct {
			items map[string]*Item
			err   error
		}
		fakeClient, srvw, srvReqs := getClientWithFakeServer(t)
		fakeClient.Timeout = time.Second
		resCh := make(chan result, 1)
		go func() {
			items, err := fakeClient.GetMulti(ctx, []string{"key1", "key2"}, WithShardKey("object-g"))
			resCh <- result{items, err}
		}()

		select {
		case req := <-srvReqs:
			if req != "gets key1 key2\r\n" {
				t.Fatalf("GetMulti sent %q, want one gets command with the item keys only", req)
			}
		case res := <-resCh:
			t.Fatalf("GetMulti returned %v before it sent a request", res.err)
		case <-time.After(5 * time.Second):
			t.Fatalf("GetMulti sent no request")
		}
		if _, err := io.WriteString(srvw, "VALUE key1 0 5 1\r\nhello\r\nSERVER_ERROR out of memory\r\n"); err != nil {
			t.Fatalf("write the server response: %v", err)
		}

		select {
		case res := <-resCh:
			if !errors.Is(res.err, ErrServerError) {
				t.Fatalf("GetMulti returned %v, want ErrServerError", res.err)
			}
			if item, ok := res.items["key1"]; !ok || string(item.Value) != "hello" {
				t.Fatalf("GetMulti returned %v, want the item that arrived before the error", res.items)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("GetMulti did not return")
		}
	})

	t.Run("GetMulti with the shard key returns ErrNoServers when the selector has no server", func(t *testing.T) {
		noServers := NewFromSelector(&ServerList{})
		t.Cleanup(noServers.Close)

		_, err := noServers.GetMulti(ctx, []string{"key"}, WithShardKey("object-h"))
		if !errors.Is(err, ErrNoServers) {
			t.Fatalf("GetMulti returned %v, want ErrNoServers", err)
		}
	})

	t.Run("GetMulti returns the items of a live server together with the error of a dead server", func(t *testing.T) {
		mixed := &ServerList{}
		if err := mixed.SetServers(addrs[0], closedAddr(t)); err != nil {
			t.Fatalf("unable to set servers in selector: %s", err)
		}
		mixedClient := NewFromSelector(mixed)
		t.Cleanup(mixedClient.Close)

		var keys []string
		if err := mixed.Each(func(addr net.Addr) error {
			keys = append(keys, keyOnServer(t, mixed, addr, "mixed"))
			return nil
		}); err != nil {
			t.Fatalf("Each: %v", err)
		}
		if err := direct[0].Set(&Item{Key: keys[0], Value: []byte("live")}); err != nil {
			t.Fatalf("Set: %v", err)
		}

		got, err := mixedClient.GetMulti(ctx, keys)
		if err == nil {
			t.Fatalf("GetMulti returned no error, want the error of the dead server")
		}
		if item, ok := got[keys[0]]; !ok || string(item.Value) != "live" {
			t.Fatalf("GetMulti returned %v, want the item of the live server", got)
		}
	})

	t.Run("GetMulti finds the items with the shard key and the allocator in either option order", func(t *testing.T) {
		keys := storeItems(t, c, servers, "alloc", 5, "object-i")
		alloc := newTestAllocator(1024)

		for _, opts := range [][]Option{
			{WithAllocator(alloc), WithShardKey("object-i")},
			{WithShardKey("object-i"), WithAllocator(alloc)},
		} {
			getsBefore := alloc.numGets
			got, err := c.GetMulti(ctx, keys, opts...)
			if err != nil {
				t.Fatalf("GetMulti: %v", err)
			}
			if len(got) != len(keys) {
				t.Fatalf("GetMulti found %d of %d items", len(got), len(keys))
			}
			if alloc.numGets == getsBefore {
				t.Fatalf("GetMulti did not use the allocator")
			}
		}
	})

	t.Run("an empty shard key picks the server of the item key", func(t *testing.T) {
		key := stringOnOtherServer(t, servers, "")
		keyAddr, err := servers.PickServer(key)
		if err != nil {
			t.Fatalf("PickServer: %v", err)
		}
		wantServer := serverIndex(t, servers, keyAddr)
		if err := c.Set(&Item{Key: key, Value: []byte("v")}, WithShardKey("")); err != nil {
			t.Fatalf("Set: %v", err)
		}

		for i, d := range direct {
			_, err := d.Get(key)
			if holds := err == nil; holds != (i == wantServer) {
				t.Fatalf("server %d holds the item: %v, want %v", i, holds, i == wantServer)
			}
		}
		if _, err := c.Get(key, WithShardKey("")); err != nil {
			t.Fatalf("Get: %v", err)
		}
	})

	t.Run("Set, Get and GetMulti accept a shard key of 250 bytes", func(t *testing.T) {
		key := "longest-shard-key-item"
		withShardKey := WithShardKey(strings.Repeat("x", 250))

		if err := c.Set(&Item{Key: key, Value: []byte("v")}, withShardKey); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if _, err := c.Get(key, withShardKey); err != nil {
			t.Fatalf("Get: %v", err)
		}
		got, err := c.GetMulti(ctx, []string{key}, withShardKey)
		if err != nil {
			t.Fatalf("GetMulti: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("GetMulti found %d of 1 items", len(got))
		}
	})

	t.Run("every method returns ErrMalformedKey and picks no server for an illegal shard key", func(t *testing.T) {
		illegalShardKeys := map[string]string{
			"a space":             "not legal",
			"a control character": "not\x01legal",
			"a line break":        "not\r\nlegal",
			"251 bytes":           strings.Repeat("x", 251),
		}

		for name, shardKey := range illegalShardKeys {
			t.Run("shard key with "+name, func(t *testing.T) {
				recorder := &recordingSelector{ServerSelector: servers}
				rc := NewFromSelector(recorder)
				t.Cleanup(rc.Close)

				withShardKey := WithShardKey(shardKey)
				newItem := func() *Item { return &Item{Key: "legal-item-key", Value: []byte("v")} }
				calls := map[string]func() error{
					"Get": func() error {
						_, err := rc.Get("legal-item-key", withShardKey)
						return err
					},
					"GetMulti": func() error {
						_, err := rc.GetMulti(ctx, []string{"legal-item-key"}, withShardKey)
						return err
					},
					"GetMulti with no item keys": func() error {
						_, err := rc.GetMulti(ctx, nil, withShardKey)
						return err
					},
					"Set":            func() error { return rc.Set(newItem(), withShardKey) },
					"Add":            func() error { return rc.Add(newItem(), withShardKey) },
					"Replace":        func() error { return rc.Replace(newItem(), withShardKey) },
					"CompareAndSwap": func() error { return rc.CompareAndSwap(newItem(), withShardKey) },
					"Touch":          func() error { return rc.Touch("legal-item-key", 100, withShardKey) },
					"Delete":         func() error { return rc.Delete("legal-item-key", withShardKey) },
					"Increment": func() error {
						_, err := rc.Increment("legal-item-key", 1, withShardKey)
						return err
					},
					"Decrement": func() error {
						_, err := rc.Decrement("legal-item-key", 1, withShardKey)
						return err
					},
				}

				for method, call := range calls {
					if err := call(); !errors.Is(err, ErrMalformedKey) {
						t.Errorf("%s returned %v, want ErrMalformedKey", method, err)
					}
				}
				recorder.mu.Lock()
				defer recorder.mu.Unlock()
				if len(recorder.picked) != 0 {
					t.Errorf("the selector picked %d times, want no pick", len(recorder.picked))
				}
			})
		}
	})

	t.Run("the last shard key option of a call wins", func(t *testing.T) {
		key := "last-wins-key"
		shardKey := stringOnOtherServer(t, servers, key)
		firstShardKey := stringOnOtherServer(t, servers, shardKey)

		item := &Item{Key: key, Value: []byte("v")}
		if err := c.Set(item, WithShardKey(firstShardKey), WithShardKey(shardKey)); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if _, err := c.Get(key, WithShardKey(shardKey)); err != nil {
			t.Fatalf("Get: %v", err)
		}
	})

	t.Run("methods with a legal shard key return ErrMalformedKey for an illegal item key", func(t *testing.T) {
		illegal := "not legal"
		withShardKey := WithShardKey("object-j")
		calls := map[string]func() error{
			"Get": func() error {
				_, err := c.Get(illegal, withShardKey)
				return err
			},
			"Set": func() error {
				return c.Set(&Item{Key: illegal, Value: []byte("v")}, withShardKey)
			},
			"Touch": func() error {
				return c.Touch(illegal, 100, withShardKey)
			},
			"Delete": func() error {
				return c.Delete(illegal, withShardKey)
			},
			"Increment": func() error {
				_, err := c.Increment(illegal, 1, withShardKey)
				return err
			},
			"Decrement": func() error {
				_, err := c.Decrement(illegal, 1, withShardKey)
				return err
			},
		}

		for name, call := range calls {
			if err := call(); !errors.Is(err, ErrMalformedKey) {
				t.Errorf("%s returned %v, want ErrMalformedKey", name, err)
			}
		}
	})
}

type recordingSelector struct {
	ServerSelector

	mu     sync.Mutex
	picked []string
}

func (s *recordingSelector) PickServer(key string) (net.Addr, error) {
	s.mu.Lock()
	s.picked = append(s.picked, key)
	s.mu.Unlock()
	return s.ServerSelector.PickServer(key)
}

func (s *recordingSelector) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.picked = nil
}

func (s *recordingSelector) pickedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.picked)
}

// storeItems stores n items with shardKey, if it is not empty, and returns
// their keys. The key of item i is prefix-i, and its value is its key. It fails
// the test when the keys all hash to one server, because GetMulti without the
// shard key then reaches one server and the test proves nothing.
func storeItems(t *testing.T, c *Client, servers *ServerList, prefix string, n int, shardKey string) []string {
	t.Helper()

	var opts []Option
	if shardKey != "" {
		opts = append(opts, WithShardKey(shardKey))
	}

	keys := make([]string, n)
	owners := map[string]struct{}{}
	for i := range keys {
		keys[i] = fmt.Sprintf("%s-%d", prefix, i)
		addr, err := servers.PickServer(keys[i])
		if err != nil {
			t.Fatalf("PickServer: %v", err)
		}
		owners[addr.String()] = struct{}{}
		if err := c.Set(&Item{Key: keys[i], Value: []byte(keys[i])}, opts...); err != nil {
			t.Fatalf("Set %q: %v", keys[i], err)
		}
	}
	if len(owners) < 2 {
		t.Fatalf("the keys with prefix %q hash to one server, want at least two", prefix)
	}
	return keys
}

// stringOnOtherServer returns a string that picks a different server than ref.
func stringOnOtherServer(t *testing.T, servers *ServerList, ref string) string {
	t.Helper()

	refAddr, err := servers.PickServer(ref)
	if err != nil {
		t.Fatalf("PickServer: %v", err)
	}
	for i := 0; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-shard-%d", ref, i)
		addr, err := servers.PickServer(candidate)
		if err != nil {
			t.Fatalf("PickServer: %v", err)
		}
		if addr.String() != refAddr.String() {
			return candidate
		}
	}
	t.Fatalf("no string picks a server other than the one of %q", ref)
	return ""
}

// keyOnServer returns a key with prefix that picks the server at addr.
func keyOnServer(t *testing.T, servers *ServerList, addr net.Addr, prefix string) string {
	t.Helper()

	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("%s-%d", prefix, i)
		picked, err := servers.PickServer(key)
		if err != nil {
			t.Fatalf("PickServer: %v", err)
		}
		if picked.String() == addr.String() {
			return key
		}
	}
	t.Fatalf("no key picks the server %s", addr)
	return ""
}

// serverIndex returns the position of addr in the server list.
func serverIndex(t *testing.T, servers *ServerList, addr net.Addr) int {
	t.Helper()

	index, found := 0, -1
	_ = servers.Each(func(a net.Addr) error {
		if a.String() == addr.String() {
			found = index
		}
		index++
		return nil
	})
	if found < 0 {
		t.Fatalf("the server %s is not in the list", addr)
	}
	return found
}

// closedAddr returns the address of a port where nothing listens.
func closedAddr(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return addr
}
