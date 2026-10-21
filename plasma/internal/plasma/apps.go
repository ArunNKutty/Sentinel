package plasma

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
//
// The guest's ne-bootstrap mounts it at AppsMount and walks apps.list.

// appsDiskSlack is headroom over the payloads for filesystem metadata.
const appsDiskSlack = 4 << 20

// appsDiskMinSize is the floor: mke2fs refuses to build an ext4 much smaller.
const appsDiskMinSize = 16 << 20

// AppsDiskPath is where a VM's applications image lives. Deterministic from
// the ID so a leftover image is recognisable after a lost run.
func AppsDiskPath(id string) string {
	return filepath.Join(RuntimeDir, id+"-apps.img")
}

// buildAppsDisk stages every app and packs them into one image for this VM.
// Returns "" when the node declares no apps, so callers can attach
// unconditionally.
func buildAppsDisk(id string, apps []AppSpec) (string, error) {
	if len(apps) == 0 {
		return "", nil
	}

	stage := filepath.Join(RunDir, "appstage", id)
	if err := os.RemoveAll(stage); err != nil {
		return "", fmt.Errorf("clear app staging: %w", err)
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return "", fmt.Errorf("app staging: %w", err)
	}
	defer os.RemoveAll(stage)

	var total int64
	list := &strings.Builder{}
	for _, a := range apps {
		dir := filepath.Join(stage, a.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("app %q: %w", a.Name, err)
		}
		file := payloadName(a)
		n, err := copyFile(a.Path, filepath.Join(dir, file))
		if err != nil {
			return "", fmt.Errorf("app %q: %w", a.Name, err)
		}
		total += n
		// A start script travels with the payload; the guest runs it as
		// ./<script> from the app's directory (see AppSpec.Cmd).
		if a.Script != "" {
			n, err := copyFile(a.Script, filepath.Join(dir, filepath.Base(a.Script)))
			if err != nil {
				return "", fmt.Errorf("app %q: start script: %w", a.Name, err)
			}
			total += n
		}
		fmt.Fprintf(list, "%s\t%s\t%s/%s\t%s\n", a.Name, a.Type, a.Name, file, a.Cmd)
	}
	if err := os.WriteFile(filepath.Join(stage, "apps.list"), []byte(list.String()), 0o644); err != nil {
		return "", fmt.Errorf("apps.list: %w", err)
	}

	if err := os.MkdirAll(RuntimeDir, 0o755); err != nil {
		return "", err
	}
	img := AppsDiskPath(id)
	os.Remove(img)
	size := total + appsDiskSlack
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
	// the daemon already has.
	out, err := exec.Command("mke2fs", "-q", "-t", "ext4",
		"-L", AppsDiskLabel, "-d", stage, "-F", img).CombinedOutput()
	if err != nil {
		os.Remove(img)
		return "", fmt.Errorf("mke2fs apps disk: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return img, nil
}

// payloadName is the payload's file name inside the app's directory. A path
// ending in a separator has no usable base name, so fall back to the app name.
func payloadName(a AppSpec) string {
	file := filepath.Base(a.Path)
	if file == "." || file == string(filepath.Separator) {
		return a.Name
	}
	return file
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
