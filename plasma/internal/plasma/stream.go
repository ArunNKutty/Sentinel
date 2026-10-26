package plasma

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Tunnels and the console are raw byte streams carried over the API
// connection: an HTTP/1.1 Upgrade to StreamProtocol, after which the
// connection is spliced to the VM. They are authenticated like any other
// call and need no port on the engine besides the API's, which is what keeps
// a lab's services off the network ("reachable only through the API").

const StreamProtocol = "plasma-stream"

// upgradeStream answers an upgrade request and hands back the raw connection.
// Anything the client sent after its request headers is still in the
// buffered reader, so it is drained first.
func upgradeStream(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), StreamProtocol) {
		http.Error(w, "this endpoint is a stream: send Upgrade: "+StreamProtocol, http.StatusUpgradeRequired)
		return nil, fmt.Errorf("not an upgrade request")
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, err
	}
	// The server's own deadlines would otherwise cut a long session.
	conn.SetDeadline(time.Time{})
	if _, err := brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + StreamProtocol +
		"\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	if err := brw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &bufferedConn{Conn: conn, r: brw.Reader}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

// Splice copies both ways until both directions end. When one side stops
// sending, the other is told with a half-close where the transport has one,
// so request-then-shutdown protocols still get their reply; otherwise both
// are closed.
func Splice(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	pipe := func(dst, src io.ReadWriteCloser) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
			src.Close()
		}
	}
	wg.Add(2)
	go pipe(a, b)
	go pipe(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

// dialVM connects to a port on a lab VM's management address, from the host.
func dialVM(ctx context.Context, ip string, port int) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(ip, fmt.Sprint(port)))
}

// openConsole opens a VM's serial line in raw mode; the restore func puts
// the pty back and closes it.
func openConsole(rec VmRecord) (*os.File, func(), error) {
	if rec.ConsolePTY == "" {
		return nil, nil, fmt.Errorf("%s has no console pty recorded", rec.ID)
	}
	f, err := os.OpenFile(rec.ConsolePTY, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open console %s: %w", rec.ConsolePTY, err)
	}
	// The pty's line discipline would echo keystrokes on top of the guest's
	// own echo, so it is put in raw mode.
	prev, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("raw mode on %s: %w", rec.ConsolePTY, err)
	}
	return f, func() { term.Restore(int(f.Fd()), prev); f.Close() }, nil
}
