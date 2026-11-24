package platform

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// currentUserSID returns the SID of the user running this process.
func currentUserSID() (*windows.SID, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read process token user: %w", err)
	}
	sid, err := tu.User.Sid.Copy()
	if err != nil {
		return nil, fmt.Errorf("copy user sid: %w", err)
	}
	return sid, nil
}

// Endpoint is the agent's named pipe: \\.\pipe\olsyn-drive-<user SID>.
func Endpoint(Paths) (string, error) {
	sid, err := currentUserSID()
	if err != nil {
		return "", err
	}
	return `\\.\pipe\olsyn-drive-` + sid.String(), nil
}

// Listen creates the named pipe with a DACL that admits only the current user.
// winio creates the first instance with FILE_FLAG_FIRST_PIPE_INSTANCE, so a
// pipe of the same name created by anyone else makes Listen fail.
func Listen(endpoint string) (net.Listener, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	s := sid.String()
	cfg := &winio.PipeConfig{
		SecurityDescriptor: "O:" + s + "D:P(A;;GA;;;" + s + ")",
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	}
	l, err := winio.ListenPipe(endpoint, cfg)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, fmt.Errorf("listen on %s: %w", endpoint, ErrAlreadyRunning)
		}
		return nil, fmt.Errorf("listen on %s: %w", endpoint, err)
	}
	return l, nil
}

// Dial connects to the agent and checks that the pipe's server runs as the
// same user, so a pipe squatted by someone else is never trusted.
func Dial(ctx context.Context, endpoint string) (net.Conn, error) {
	conn, err := winio.DialPipeContext(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", endpoint, err)
	}
	if err := checkPipeServer(conn); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func checkPipeServer(conn net.Conn) error {
	f, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("pipe connection exposes no handle to verify")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(f.Fd()), &pid); err != nil {
		return fmt.Errorf("find pipe server process: %w", err)
	}
	proc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return fmt.Errorf("open pipe server process: %w", err)
	}
	defer windows.CloseHandle(proc)
	var tok windows.Token
	if err := windows.OpenProcessToken(proc, windows.TOKEN_QUERY, &tok); err != nil {
		return fmt.Errorf("open pipe server token: %w", err)
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read pipe server user: %w", err)
	}
	me, err := currentUserSID()
	if err != nil {
		return err
	}
	if !tu.User.Sid.Equals(me) {
		return errors.New("the agent pipe belongs to another user; refusing to use it")
	}
	return nil
}
