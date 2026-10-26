package plasma

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Applications declared by a topology node are delivered on a small per-VM
// ext4 disk rather than copied into the VM's rootfs. The rootfs is a
// copy-on-write clone of a 4GB base image, so writing into it means opening a
// loop mount as root per VM; a separate image is built from a staging
// directory with `mke2fs -d`, needs no mount, and is attached read-only like
// the fleet-wide artifacts disk.
//
// Layout inside the image:
//
//	apps.list          name<TAB>type<TAB>relative path<TAB>cmd, one per app
//	<name>/<file>      the payload itself, mode 0755
//	<name>/<script>    the start script, when cmd named one
//	<name>/...         a bundle's contents, unpacked (its path is "<name>/.")
//
// The guest's ne-bootstrap mounts it at AppsMount and walks apps.list.

// appsDiskSlack is headroom over the payloads for filesystem metadata: a
// floor, plus a share of the payload (extent blocks) and a block per file
// (each file's last block is partly empty).
const (
	appsDiskSlack   = 16 << 20
	appsDiskPerFile = 8 << 10
)

// appsDiskMinSize is the floor: mke2fs refuses to build an ext4 much smaller.
const appsDiskMinSize = 16 << 20

// buildAppsDisk stages every app and packs them into one image at img.
// Returns "" when the node declares no apps, so callers can attach
// unconditionally.
func buildAppsDisk(img string, apps []AppSpec) (string, error) {
	if len(apps) == 0 {
		return "", nil
	}

	if err := os.MkdirAll(RunDir, 0o755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(RunDir, "appstage-")
	if err != nil {
		return "", fmt.Errorf("app staging: %w", err)
	}
	defer os.RemoveAll(stage)

	var total int64
	files := 0
	list := &strings.Builder{}
	for _, a := range apps {
		dir := filepath.Join(stage, a.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("app %q: %w", a.Name, err)
		}
		file := payloadName(a)
		var n int64
		if a.Type == AppTypeBundle {
			// "<name>/." keeps the guest bootstrap unchanged: it runs cmd from
			// dirname($APP), which is then the unpacked bundle itself.
			var c int
			n, c, err = extractBundle(a.Path, dir)
			files += c
			file = "."
		} else {
			n, err = copyFile(a.Path, filepath.Join(dir, file))
			files++
		}
		if err != nil {
			return "", fmt.Errorf("app %q: %w", a.Name, err)
		}
		total += n
		// A start script travels with the payload; the guest runs it as
		// ./<script> from the app's directory (see AppSpec.Cmd).
		if a.Script != "" {
			n, err := copyFile(a.Script, filepath.Join(dir, scriptName(a)))
			if err != nil {
				return "", fmt.Errorf("app %q: start script: %w", a.Name, err)
			}
			total += n
			files++
		}
		fmt.Fprintf(list, "%s\t%s\t%s/%s\t%s\n", a.Name, a.Type, a.Name, file, a.Cmd)
	}
	if err := os.WriteFile(filepath.Join(stage, "apps.list"), []byte(list.String()), 0o644); err != nil {
		return "", fmt.Errorf("apps.list: %w", err)
	}

	os.Remove(img)
	files += 2 * len(apps) // directories, apps.list
	size := total + total/32 + int64(files)*appsDiskPerFile + appsDiskSlack
	if size < appsDiskMinSize {
		size = appsDiskMinSize
	}
	f, err := os.Create(img)
	if err != nil {
		return "", fmt.Errorf("create apps disk: %w", err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return "", fmt.Errorf("size apps disk: %w", err)
	}
	f.Close()

	// -d populates from the staging tree, so no mount and no root beyond what
	// the daemon already has. The guest mounts it read-only, so it needs no
	// journal (which on a multi-GB image would outgrow the slack) and no
	// blocks reserved for root; -N makes room for a bundle's many files.
	out, err := exec.Command("mke2fs", "-q", "-t", "ext4", "-O", "^has_journal", "-m", "0",
		"-N", strconv.Itoa(files+1024),
		"-L", AppsDiskLabel, "-d", stage, "-F", img).CombinedOutput()
	if err != nil {
		os.Remove(img)
		return "", fmt.Errorf("mke2fs apps disk: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return img, nil
}

// payloadName is the payload's file name inside the app's directory: the name
// it was written under, even when Path is a content-addressed store entry. A
// path ending in a separator has no usable base name, so fall back to the app
// name.
func payloadName(a AppSpec) string {
	if a.FileName != "" {
		return a.FileName
	}
	file := filepath.Base(a.Path)
	if file == "." || file == string(filepath.Separator) {
		return a.Name
	}
	return file
}

func scriptName(a AppSpec) string {
	if a.ScriptName != "" {
		return a.ScriptName
	}
	return filepath.Base(a.Script)
}

// copyFile copies src to dst as an executable, returning the bytes written.
// Everything on the disk is 0755: a payload is either a program to run or a
// module to load, and a non-executable copy of either is a boot-time failure
// that is tedious to diagnose from a serial console.
func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriter(out)
	n, err := io.Copy(w, in)
	if err == nil {
		err = w.Flush()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}
