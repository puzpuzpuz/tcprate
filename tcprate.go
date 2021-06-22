package tcprate

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Listener is rate limited wrapper for net.Listener.
// One can use it to build a TCP server that throttles its traffic.
// The rate limit is only applied to outbound traffic.
//
// Instances of Listener should be created with NewListener.
//
// All methods of this struct are thread-safe.
//
// The underlying algorithm is based on token buckets.
// See https://en.wikipedia.org/wiki/Token_bucket for more about token buckets.
type Listener struct {
	wrapped      net.Listener
	mu           sync.RWMutex  // protects the below fields
	lim          *rate.Limiter // global limiter
	conns        map[*conn]bool
	limit        int
	perConnLimit int
}

// NewListener creates and returns a new rate limited wrapper for the given
// net.Listener.
func NewListener(l net.Listener) *Listener {
	return &Listener{
		wrapped: l,
		lim:     newLimiter(0),
		conns:   make(map[*conn]bool),
	}
}

// SetLimits changes the limits applied to the listener.
//
// The limit value defines the per server rate limit, preserved accross all
// client connections, in bytes per second. Zero means unlimited rate.
//
// The perConnLimit value defines the per client connection rate limit, in
// bytes per second. Zero means unlimited rate.
//
// Applies to both established and future client connections.
func (l *Listener) SetLimits(limit, perConnLimit int) error {
	if limit < 0 {
		return fmt.Errorf("negative limit provided: %d", limit)
	}
	if perConnLimit < 0 {
		return fmt.Errorf("negative perConnLimit provided: %d", perConnLimit)
	}
	if limit > 0 && limit < perConnLimit {
		return fmt.Errorf("limit has to be not less than perConnLimit: limit=%d, perConnLimit=%d",
			limit, perConnLimit)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.limit = limit
	l.perConnLimit = perConnLimit

	l.lim = newLimiter(limit)
	for c := range l.conns {
		c.mu.Lock()
		c.lim = newLimiter(perConnLimit)
		c.mu.Unlock()
	}
	return nil
}

// Accept waits for and returns the next connection to the listener.
func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.wrapped.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	conn := &conn{
		lis:     l,
		wrapped: c,
		lim:     newLimiter(l.perConnLimit),
	}
	l.conns[conn] = true
	return conn, nil
}

// Close closes the listener.
// Any blocked Accept operations will be unblocked and return errors.
func (l *Listener) Close() error {
	return l.wrapped.Close()
}

// Addr returns the listener's network address.
func (l *Listener) Addr() net.Addr {
	return l.wrapped.Addr()
}

func newLimiter(limit int) *rate.Limiter {
	if limit > 0 {
		return rate.NewLimiter(rate.Limit(limit), limit)
	}
	return rate.NewLimiter(rate.Inf, 0)
}

// conn is rate limited wrapper for net.Conn.
type conn struct {
	lis      *Listener
	wrapped  net.Conn
	mu       sync.Mutex    // protects the below fields
	lim      *rate.Limiter // per conn limiter
	deadline time.Time
}

func (c *conn) Read(b []byte) (n int, err error) {
	return c.wrapped.Read(b)
}

func (c *conn) Write(b []byte) (n int, err error) {
	c.lis.mu.RLock()
	globalLim := c.lis.lim
	limit := c.lis.limit
	perConnLimit := c.lis.perConnLimit
	c.lis.mu.RUnlock()

	c.mu.Lock()
	localLim := c.lim
	deadline := c.deadline
	c.mu.Unlock()

	nwrite := len(b)
	if limit > 0 && nwrite > limit {
		nwrite = limit
	}
	if perConnLimit > 0 && nwrite > perConnLimit {
		nwrite = perConnLimit
	}

	ctx := context.Background()
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(context.Background(), deadline)
		defer cancel()
	}

	nwrote := 0
	for nwrote != len(b) {
		// Spend up to nwrite tokens upfront.
		if err := globalLim.WaitN(ctx, nwrite); err != nil {
			return nwrote, fmt.Errorf("deadline exceeded: %v", os.ErrDeadlineExceeded)
		}
		if err := localLim.WaitN(ctx, nwrite); err != nil {
			return nwrote, fmt.Errorf("deadline exceeded: %v", os.ErrDeadlineExceeded)
		}
		// Do the actual write.
		end := nwrote + nwrite
		if end > len(b) {
			end = len(b)
		}
		n, err := c.wrapped.Write(b[nwrote:end])
		nwrote += n
		if err != nil {
			return nwrote, err
		}
	}
	return nwrote, nil
}

func (c *conn) Close() error {
	c.lis.mu.Lock()
	delete(c.lis.conns, c)
	c.lis.mu.Unlock()
	return c.wrapped.Close()
}

func (c *conn) LocalAddr() net.Addr {
	return c.wrapped.LocalAddr()
}

func (c *conn) RemoteAddr() net.Addr {
	return c.wrapped.RemoteAddr()
}

func (c *conn) SetDeadline(t time.Time) error {
	err := c.wrapped.SetDeadline(t)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if t.After(c.deadline) {
		c.deadline = t
	}
	c.mu.Unlock()
	return nil
}

func (c *conn) SetReadDeadline(t time.Time) error {
	return c.wrapped.SetReadDeadline(t)
}

func (c *conn) SetWriteDeadline(t time.Time) error {
	err := c.wrapped.SetWriteDeadline(t)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if t.After(c.deadline) {
		c.deadline = t
	}
	c.mu.Unlock()
	return nil
}
