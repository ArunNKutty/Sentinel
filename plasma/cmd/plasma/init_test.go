package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFakeBzImage builds just enough of a bzImage for kernelRelease: a
// two-byte pointer at 0x20E holding the version string's offset relative to
// 0x200.
func writeFakeBzImage(t *testing.T, release string) string {
	t.Helper()
	const strOff = 0x400 // absolute file offset for the version string
	img := make([]byte, 0x600)

	ptr := strOff - 0x200
	img[0x20E] = byte(ptr & 0xff)
	img[0x20F] = byte(ptr >> 8)
	copy(img[strOff:], append([]byte(release), 0))

	path := filepath.Join(t.TempDir(), "vmlinuz")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	return path
}

func TestKernelReleaseReadsHeaderPointer(t *testing.T) {
	// The release, not the Debian package version, is what a module's vermagic
	// must match — so this has to come from the header pointer rather than a
	// scan for something version-shaped.
	path := writeFakeBzImage(t, "6.18.10+deb14-cloud-amd64 (builder@host) #1 SMP")

	if got := kernelRelease(path); got != "6.18.10+deb14-cloud-amd64" {
		t.Errorf("kernelRelease = %q, want the release with build details stripped", got)
	}
}

func TestKernelReleaseMissingFile(t *testing.T) {
	if got := kernelRelease(filepath.Join(t.TempDir(), "absent")); got != "" {
		t.Errorf("kernelRelease on a missing file = %q, want \"\"", got)
	}
}

func TestKernelReleaseNoVersionPointer(t *testing.T) {
	// A file that is not a bzImage must degrade gracefully, not report junk as
	// though it were a kernel version.
	path := filepath.Join(t.TempDir(), "notakernel")
	if err := os.WriteFile(path, make([]byte, 0x600), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := kernelRelease(path); got != "installed" {
		t.Errorf("kernelRelease with a zero pointer = %q, want \"installed\"", got)
	}
}
