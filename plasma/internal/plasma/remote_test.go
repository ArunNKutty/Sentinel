package plasma

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// remoteEngine is a plasmad network listener over test state: real TLS, real
// token check, real handlers, no root needed.
type remoteEngine struct {
	url         string
	token       string
	fingerprint string
	store       *ArtifactStore
	state       *State
	labs        *Labs
}

func startRemoteEngine(t *testing.T) *remoteEngine {
	t.Helper()
	dir := t.TempDir()
	state, err := LoadState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := NewArtifactStore(filepath.Join(dir, "artifacts"))
	var mu sync.Mutex
	labs, err := LoadLabs(filepath.Join(dir, "labs.json"), state, store, &mu, log)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := EnsureCert(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := EnsureToken(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(state, labs, store, &mu, ServerConfig{}, log)
	ln, hs, err := srv.ListenTCP("127.0.0.1:0", cert, token)
	if err != nil {
		t.Fatal(err)
	}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })
	return &remoteEngine{
		url: "https://" + ln.Addr().String(), token: token,
		fingerprint: Fingerprint(cert.Certificate[0]), store: store, state: state, labs: labs,
	}
}

func (e *remoteEngine) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewRemoteClient(EngineConfig{URL: e.url, Token: e.token, Fingerprint: e.fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRemoteEngineNeedsTheToken(t *testing.T) {
	e := startRemoteEngine(t)
	if _, err := e.client(t).Engine(context.Background()); err != nil {
		t.Fatalf("with the token: %v", err)
	}
	bad, _ := NewRemoteClient(EngineConfig{URL: e.url, Token: "wrong", Fingerprint: e.fingerprint})
	if _, err := bad.Engine(context.Background()); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("wrong token: err = %v, want a token rejection", err)
	}
}

func TestRemoteEngineChecksThePin(t *testing.T) {
	e := startRemoteEngine(t)
	other := "sha256:" + strings.Repeat("ab", 32)
	c, _ := NewRemoteClient(EngineConfig{URL: e.url, Token: e.token, Fingerprint: other})
	if _, err := c.Engine(context.Background()); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Errorf("wrong pin: err = %v, want a pin mismatch", err)
	}
	fp, err := FetchFingerprint(e.url)
	if err != nil || fp != e.fingerprint {
		t.Errorf("FetchFingerprint = %q, %v; want %q", fp, err, e.fingerprint)
	}
}

func TestArtifactsAreContentAddressed(t *testing.T) {
	e := startRemoteEngine(t)
	c := e.client(t)
	f := filepath.Join(t.TempDir(), "neapp")
	os.WriteFile(f, []byte("payload"), 0o755)

	d, sent, err := c.PushArtifact(context.Background(), f)
	if err != nil || !sent {
		t.Fatalf("first push: sent=%v err=%v", sent, err)
	}
	if _, sent, err = c.PushArtifact(context.Background(), f); err != nil || sent {
		t.Errorf("second push: sent=%v err=%v; want a cache hit", sent, err)
	}
	if p, err := e.store.Open(ArtifactRef(d)); err != nil {
		t.Errorf("store does not hold it: %v", err)
	} else if b, _ := os.ReadFile(p); string(b) != "payload" {
		t.Errorf("stored %q", b)
	}

	// Content that does not hash to its name is refused.
	wrong := strings.Repeat("0", 64)
	req, _ := http.NewRequest(http.MethodPut, e.url+"/v1/artifacts/"+wrong, strings.NewReader("x"))
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("mismatched upload: %s, want 400", resp.Status)
	}
	if _, err := e.store.Stat(wrong); !errors.Is(err, ErrNoArtifact) {
		t.Errorf("mismatched upload was kept: %v", err)
	}
}

// TestTunnelReachesALabVM runs a stand-in VM (an echo server on 127.0.0.1)
// and reaches it through the engine, over TLS, the way `plasma lab tunnel`
// does.
func TestTunnelReachesALabVM(t *testing.T) {
	e := startRemoteEngine(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(conn, conn); conn.Close() }()
		}
	}()
	port := echo.Addr().(*net.TCPAddr).Port

	e.labs.labs["it-1"] = &LabRecord{ID: "it-1", Index: 1, Phase: LabRunning, Nodes: []string{"fabric"}}
	e.state.Add(VmRecord{Lab: "it-1", ID: "fabric", Status: StatusRunning, PID: os.Getpid(), IP: "127.0.0.1"})

	c := e.client(t)
	stream, err := c.Tunnel(context.Background(), "it-1", "fabric", port)
	if err != nil {
		t.Fatalf("tunnel: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Write([]byte("hello lab")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("hello lab"))
	if _, err := io.ReadFull(stream, got); err != nil || string(got) != "hello lab" {
		t.Errorf("echo = %q, %v", got, err)
	}

	// A VM of another lab, or a port nothing listens on, is an HTTP error.
	if _, err := c.Tunnel(context.Background(), "it-2", "fabric", port); err == nil {
		t.Error("tunnel into a missing lab succeeded")
	}
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := closed.Addr().(*net.TCPAddr).Port
	closed.Close()
	if _, err := c.Tunnel(context.Background(), "it-1", "fabric", dead); err == nil ||
		!strings.Contains(err.Error(), strconv.Itoa(dead)) {
		t.Errorf("tunnel to a closed port: %v", err)
	}
}

func TestLabStatusReportsReadyFromTheConsole(t *testing.T) {
	e := startRemoteEngine(t)
	e.labs.labs["it-1"] = &LabRecord{ID: "it-1", Index: 1, Phase: LabRunning, Nodes: []string{"a"}}
	start := time.Now()
	e.state.Add(VmRecord{Lab: "it-1", ID: "a", Status: StatusRunning, PID: os.Getpid(), StartedAt: start})

	st, err := e.client(t).Lab(context.Background(), "it-1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Ready || len(st.VMs) != 1 || st.VMs[0].Ready {
		t.Errorf("ready before any marker: %+v", st)
	}
	if st.Mgmt.Subnet != "10.0.1.0/24" || st.Mgmt.Bridge != "plasma-br1" {
		t.Errorf("mgmt = %+v", st.Mgmt)
	}

	// consoleHasMarker is what decides; the console path itself is under
	// /var/run, so exercise the reader on a file of our own.
	log := filepath.Join(t.TempDir(), "console.log")
	os.WriteFile(log, []byte("Debian login: \nPLASMA-READY apps=2\n"), 0o644)
	if !consoleHasMarker(log) {
		t.Error("marker not found")
	}
	if consoleHasMarker(filepath.Join(t.TempDir(), "absent")) {
		t.Error("marker found in a missing file")
	}
}

func TestLabTopologyResolvesAgainstArtifacts(t *testing.T) {
	store := NewArtifactStore(t.TempDir())
	put := func(content string) string {
		sum := sha256.Sum256([]byte(content))
		d := hex.EncodeToString(sum[:])
		if _, err := store.Put(d, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		return ArtifactRef(d)
	}
	ko, app, script := put("ko"), put("app"), put("#!/bin/sh")
	bundle := put(string(tarball(t, map[string]string{"boot.sh": "#!/bin/sh\n"})))

	yaml := `
name: it
topology:
  defaults: {kind: vm}
  nodes:
    fabric:
      apps:
        - {name: gluon, path: ../dist/gluon_sim.ko}
        - {name: neapp, path: ./../dist/neapp, cmd: ./apps/start.sh}
        - {name: it, artifact: ` + bundle + `, cmd: ./boot.sh}
`
	files := map[string]string{"../dist/gluon_sim.ko": ko, "../dist/neapp": app, "apps/start.sh": script}
	topo, err := ResolveLabTopology([]byte(yaml), files, store)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	apps := topo.Nodes[0].Apps
	if apps[0].Type != AppTypeKernel || apps[0].FileName != "gluon_sim.ko" {
		t.Errorf("module = %+v", apps[0])
	}
	if apps[1].FileName != "neapp" || apps[1].ScriptName != "start.sh" || apps[1].Cmd != "./start.sh" {
		t.Errorf("app = %+v", apps[1])
	}
	if apps[2].Type != AppTypeBundle || apps[2].Cmd != "./boot.sh" || apps[2].Script != "" {
		t.Errorf("bundle = %+v; its cmd names a file inside it", apps[2])
	}

	delete(files, "apps/start.sh")
	if _, err := ResolveLabTopology([]byte(yaml), files, store); err == nil || !strings.Contains(err.Error(), "apps/start.sh") {
		t.Errorf("missing upload: err = %v, want it named", err)
	}
}

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func writeTar(t *testing.T, entries []tar.Header, bodies map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		h := h
		if b, ok := bodies[h.Name]; ok {
			h.Size = int64(len(b))
		}
		tw.WriteHeader(&h)
		if b, ok := bodies[h.Name]; ok {
			tw.Write([]byte(b))
		}
	}
	tw.Close()
	p := filepath.Join(t.TempDir(), "b.tar")
	os.WriteFile(p, buf.Bytes(), 0o644)
	return p
}

func TestExtractBundle(t *testing.T) {
	src := writeTar(t, []tar.Header{
		{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "bin/atom", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "boot.sh", Typeflag: tar.TypeReg, Mode: 0o755},
		{Name: "lib.so", Typeflag: tar.TypeSymlink, Linkname: "bin/atom"},
	}, map[string]string{"bin/atom": "elf", "boot.sh": "#!/bin/sh"})
	dir := t.TempDir()
	if _, n, err := extractBundle(src, dir); err != nil || n != 4 {
		t.Fatalf("extract: %d entries, %v", n, err)
	}
	if st, err := os.Stat(filepath.Join(dir, "boot.sh")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("boot.sh: %v %v", st, err)
	}
	if l, err := os.Readlink(filepath.Join(dir, "lib.so")); err != nil || l != "bin/atom" {
		t.Errorf("symlink: %q %v", l, err)
	}
}

func TestExtractBundleRefusesEscapes(t *testing.T) {
	outside := t.TempDir()
	hostile := map[string][]tar.Header{
		"dotdot":   {{Name: "../evil", Typeflag: tar.TypeReg}},
		"absolute": {{Name: "/etc/evil", Typeflag: tar.TypeReg}},
		"device":   {{Name: "dev", Typeflag: tar.TypeChar}},
		"hardlink": {{Name: "h", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}},
		// A link, then a file through it: the file must not land outside.
		"through-link": {
			{Name: "x", Typeflag: tar.TypeSymlink, Linkname: outside},
			{Name: "x/evil", Typeflag: tar.TypeReg},
		},
		"link-chain": {
			{Name: "x", Typeflag: tar.TypeSymlink, Linkname: outside},
			{Name: "x/y", Typeflag: tar.TypeSymlink, Linkname: "z"},
		},
	}
	for name, entries := range hostile {
		src := writeTar(t, entries, map[string]string{"../evil": "x", "/etc/evil": "x", "x/evil": "x"})
		if _, _, err := extractBundle(src, t.TempDir()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if left, _ := os.ReadDir(outside); len(left) > 0 {
		t.Errorf("a hostile bundle wrote outside its directory: %v", left)
	}
}

func TestNetemArgs(t *testing.T) {
	cases := map[string]struct {
		f    LinkFilter
		want string
	}{
		"delay":  {LinkFilter{Delay: "50ms", Jitter: "5ms"}, "delay 50000us 5000us"},
		"loss":   {LinkFilter{Loss: 0.5}, "loss 0.5%"},
		"drop":   {LinkFilter{Drop: true, Loss: 3}, "loss 100%"},
		"both":   {LinkFilter{Delay: "1s", Loss: 10}, "delay 1000000us loss 10%"},
		"empty":  {LinkFilter{}, ""},
		"neg":    {LinkFilter{Loss: -1}, ""},
		"jitter": {LinkFilter{Jitter: "5ms"}, ""},
		"inject": {LinkFilter{Delay: "1ms; reboot"}, ""},
		"long":   {LinkFilter{Delay: "2h"}, ""},
	}
	for name, c := range cases {
		args, err := c.f.netemArgs()
		got := strings.Join(args, " ")
		if c.want == "" && err == nil {
			t.Errorf("%s: accepted, args %q", name, got)
		}
		if c.want != "" && (err != nil || got != c.want) {
			t.Errorf("%s: %q, %v; want %q", name, got, err, c.want)
		}
	}
}

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]int64{"1024M": 1 << 30, "2G": 2 << 30, "512k": 512 << 10, "": 1 << 30} {
		if got, err := ParseMemory(in); err != nil || got != want {
			t.Errorf("ParseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"lots", "-1G", "0M"} {
		if _, err := ParseMemory(bad); err == nil {
			t.Errorf("ParseMemory(%q) accepted", bad)
		}
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	want := "sha256:" + strings.Repeat("ab", 32)
	for _, in := range []string{want, strings.Repeat("AB", 32), "SHA256 Fingerprint=" + strings.TrimSuffix(strings.Repeat("AB:", 32), ":")} {
		if got, err := NormalizeFingerprint(in); err != nil || got != want {
			t.Errorf("NormalizeFingerprint(%q) = %q, %v", in, got, err)
		}
	}
}

// Over the network, a legacy launch must not make the root daemon read a
// file of the engine's choosing into a VM.
func TestNetworkLaunchRefusesHostPaths(t *testing.T) {
	e := startRemoteEngine(t)
	_, err := e.client(t).LaunchSpec(LaunchRequest{Group: "x", ID: "x",
		Apps: []AppSpec{{Name: "a", Type: AppTypeUserspace, Path: "/etc/shadow", Cmd: "$APP"}}})
	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusForbidden {
		t.Errorf("err = %v, want 403", err)
	}
}

// A client checking a lab topology has not seen its artifacts; it must not
// reject one whose cmd lives inside a bundle.
func TestClientAcceptsArtifactApps(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "neapp"), []byte("x"), 0o755)
	topo := filepath.Join(dir, "lab.yaml")
	os.WriteFile(topo, []byte(`
name: it
topology:
  defaults: {kind: vm}
  nodes:
    fabric:
      apps:
        - {name: it, artifact: sha256:`+strings.Repeat("a", 64)+`, cmd: ./boot.sh}
        - {name: neapp, path: neapp}
`), 0o644)
	got, err := LoadLabTopoFile(topo)
	if err != nil {
		t.Fatalf("client check: %v", err)
	}
	if len(got.Files) != 1 || got.Files["neapp"] == "" {
		t.Errorf("files to upload = %v, want just neapp", got.Files)
	}
	if _, err := LoadTopoFile(topo); err == nil {
		t.Error("the legacy loader accepted an artifact")
	}
}

// The drop rule must produce exactly the u32 filter fault-injector.sh
// installs for a handshake datagram (offsets 31/34, type 0x5).
func TestDropRuleMatchesTheFaultInjector(t *testing.T) {
	r := DropRule{Protocol: 17, Src: "10.77.0.1", Sport: 4242,
		Bytes: []ByteMatch{{Offset: 31, Value: 0x42}, {Offset: 34, Value: 5}}}
	args, err := r.u32Args()
	if err != nil {
		t.Fatal(err)
	}
	want := "protocol ip u32 match ip protocol 17 0xff match ip src 10.77.0.1/32 match ip sport 4242 0xffff " +
		"match u8 0x42 0xff at 31 match u8 0x5 0xff at 34 action drop"
	if got := strings.Join(args, " "); got != want {
		t.Errorf("u32 = %q\nwant  %q", got, want)
	}
	bad := []DropRequest{
		{},
		{Rules: []DropRule{{}}},
		{Rules: []DropRule{{Src: "10.77.0.1; reboot"}}},
		{Rules: []DropRule{{Bytes: []ByteMatch{{Offset: 9999, Value: 1}}}}},
		{Rules: []DropRule{{Sport: 70000}}},
		{Rules: []DropRule{{Protocol: 17}}, Timeout: "1h"},
	}
	for _, q := range bad {
		if err := q.validate(); err == nil {
			t.Errorf("accepted %+v", q)
		}
	}
}

func TestLocalOnlyIfaces(t *testing.T) {
	for _, name := range []string{"docker0", "br-3f2a9c1e5b7d", "veth12ab", "virbr0", "plasma-br3", "pt3-7-eth1", "cni0"} {
		if !localOnlyIface(name) {
			t.Errorf("%s offered to clients", name)
		}
	}
	for _, name := range []string{"eth0", "enp1s0", "eno1", "wlp2s0", "bond0"} {
		if localOnlyIface(name) {
			t.Errorf("%s hidden from clients", name)
		}
	}
}
