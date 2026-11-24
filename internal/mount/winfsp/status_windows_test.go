//go:build windows

package winfsp

import (
	"fmt"
	"testing"

	"github.com/geyervalmont/olsyn-drive-client/internal/vfs"
	"golang.org/x/sys/windows"
)

func TestEveryKindHasAStatus(t *testing.T) {
	for k := vfs.KindOther; k <= vfs.KindInvalid; k++ {
		if _, ok := kindStatus[k]; !ok {
			t.Errorf("no NTSTATUS for %v", k)
		}
	}
	err := fmt.Errorf("wrapped: %w", &vfs.Error{Kind: vfs.KindOffline, Op: "read"})
	if toStatus(err) != windows.STATUS_NETWORK_UNREACHABLE {
		t.Fatal("offline must be STATUS_NETWORK_UNREACHABLE")
	}
}
