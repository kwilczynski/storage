//go:build linux && !btrfs_noversion && cgo

package btrfs

import (
	"testing"
)

func TestLibVersion(t *testing.T) {
	if btrfsLibVersion() <= 0 {
		t.Error("expected output from btrfs lib version > 0")
	}
}
