package agentconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// PipeDialer connects to the agent's named pipe and checks that the process
// serving it runs as this same user (docs/ipc.md: the client checks the
// pipe's server process SID), so a pipe squatted by another account is refused.
type PipeDialer struct {
	// Name is the full pipe path, e.g. \\.\pipe\olsyn-drive-S-1-5-21-....
	Name string
}

// DefaultDialer returns the dialer for this user's agent pipe.
func DefaultDialer() (Dialer, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	return PipeDialer{Name: PipeName(sid.String())}, nil
}

// PipeName is the agent's pipe for a user SID.
func PipeName(sid string) string {
	return `\\.\pipe\olsyn-drive-` + sid
}

// Endpoint returns the pipe path.
func (d PipeDialer) Endpoint() string { return d.Name }

// DialContext opens the pipe with overlapped I/O (so reads and writes can be in
// flight together and deadlines work), retrying while every instance is busy.
func (d PipeDialer) DialContext(ctx context.Context) (net.Conn, error) {
	name, err := windows.UTF16PtrFromString(d.Name)
	if err != nil {
		return nil, fmt.Errorf("pipe name: %w", err)
	}
	for {
		h, err := windows.CreateFile(name,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION,
			0)
		if err == nil {
			if verr := verifyServerIsCurrentUser(h); verr != nil {
				windows.CloseHandle(h)
				return nil, verr
			}
			f := os.NewFile(uintptr(h), d.Name)
			if f == nil {
				windows.CloseHandle(h)
				return nil, errors.New("open agent pipe: invalid handle")
			}
			return &pipeConn{f: f, name: d.Name}, nil
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, &DialError{Endpoint: d.Name, Err: err}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("open agent pipe %s: %w", d.Name, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read this user's SID: %w", err)
	}
	return user.User.Sid, nil
}

func verifyServerIsCurrentUser(pipe windows.Handle) error {
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(pipe, &pid); err != nil {
		return fmt.Errorf("find the agent pipe's server process: %w", err)
	}
	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fmt.Errorf("open the agent pipe's server process: %w", err)
	}
	defer windows.CloseHandle(proc)
	var token windows.Token
	if err := windows.OpenProcessToken(proc, windows.TOKEN_QUERY, &token); err != nil {
		return fmt.Errorf("read the agent pipe's server token: %w", err)
	}
	defer token.Close()
	serverUser, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read the agent pipe's server user: %w", err)
	}
	me, err := currentUserSID()
	if err != nil {
		return err
	}
	if !serverUser.User.Sid.Equals(me) {
		return errors.New("the agent pipe is served by another user's process; refusing to use it")
	}
	return nil
}

// pipeConn adapts an overlapped pipe *os.File to net.Conn.
type pipeConn struct {
	f    *os.File
	name string
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

func (c *pipeConn) Read(b []byte) (int, error)  { return c.f.Read(b) }
func (c *pipeConn) Write(b []byte) (int, error) { return c.f.Write(b) }
func (c *pipeConn) Close() error                { return c.f.Close() }
func (c *pipeConn) LocalAddr() net.Addr         { return pipeAddr(c.name) }
func (c *pipeConn) RemoteAddr() net.Addr        { return pipeAddr(c.name) }
func (c *pipeConn) SetDeadline(t time.Time) error {
	return c.f.SetDeadline(t)
}
func (c *pipeConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }
