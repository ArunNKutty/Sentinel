package plasma

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Artifacts are the files a remote client hands the engine: payloads, start
// scripts, bundles. They are stored by content hash, so a rerun uploads only
// what changed (HEAD first, PUT what is missing) and two labs using the same
// build share one copy. Nothing is ever evicted in phase 1.

// ArtifactsDir sits beside state: it outlives a reboot, and plasma exit keeps
// it, as a cache.
const ArtifactsDir = "/var/lib/plasma/artifacts"

// MaxArtifactBytes caps a single upload.
const MaxArtifactBytes = 32 << 30

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrNoArtifact means the engine does not hold an artifact.
var ErrNoArtifact = errors.New("artifact not on the engine")

// ParseArtifactRef accepts "sha256:<hex>" or a bare hex digest and returns
// the digest.
func ParseArtifactRef(ref string) (string, error) {
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ref)), "sha256:")
	if !hexDigest.MatchString(d) {
		return "", fmt.Errorf("artifact %q: want sha256:<64 hex digits>", ref)
	}
	return d, nil
}

// ArtifactRef formats a digest the way topologies write it.
func ArtifactRef(digest string) string { return "sha256:" + digest }

type ArtifactStore struct{ dir string }

func NewArtifactStore(dir string) *ArtifactStore { return &ArtifactStore{dir: dir} }

func (s *ArtifactStore) path(digest string) string {
	return filepath.Join(s.dir, "sha256", digest)
}

// Stat reports an artifact's size, or ErrNoArtifact.
func (s *ArtifactStore) Stat(digest string) (int64, error) {
	st, err := os.Stat(s.path(digest))
	if os.IsNotExist(err) {
		return 0, fmt.Errorf("%w: %s", ErrNoArtifact, ArtifactRef(digest))
	}
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Open resolves a reference to the stored file's path.
func (s *ArtifactStore) Open(ref string) (string, error) {
	d, err := ParseArtifactRef(ref)
	if err != nil {
		return "", err
	}
	if _, err := s.Stat(d); err != nil {
		return "", err
	}
	return s.path(d), nil
}

// Put stores r under digest, refusing content that does not hash to it.
// Written to a temp file and renamed, so a reader never sees a partial
// artifact and a failed upload leaves nothing behind.
func (s *ArtifactStore) Put(digest string, r io.Reader) (int64, error) {
	dir := filepath.Join(s.dir, "sha256")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return n, fmt.Errorf("content hashes to sha256:%s, not sha256:%s", got, digest)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return n, err
	}
	return n, os.Rename(tmp.Name(), s.path(digest))
}

// Usage counts what the store holds.
func (s *ArtifactStore) Usage() (count int, bytes int64) {
	entries, _ := os.ReadDir(filepath.Join(s.dir, "sha256"))
	for _, e := range entries {
		if !hexDigest.MatchString(e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil {
			count++
			bytes += info.Size()
		}
	}
	return count, bytes
}

// labFiles resolves a lab topology's files on the engine: every written path
// must appear in the files map the client sent, pointing at an artifact the
// store holds.
type labFiles struct {
	files map[string]string // FileKey -> sha256:<hex>
	store *ArtifactStore
}

func (l *labFiles) file(written string) (string, error) {
	ref, ok := l.files[FileKey(written)]
	if !ok {
		return "", fmt.Errorf("%s: %w (not in the lab's files; `plasma lab apply` uploads it)", written, errNoFile)
	}
	p, err := l.store.Open(ref)
	if err != nil {
		return "", fmt.Errorf("%s: %w", written, err)
	}
	return p, nil
}

func (l *labFiles) artifact(ref string) (string, error) { return l.store.Open(ref) }

// ResolveLabTopology resolves a topology on the engine, against uploaded
// artifacts rather than the engine's disk.
func ResolveLabTopology(data []byte, files map[string]string, store *ArtifactStore) (*ResolvedTopology, error) {
	f, err := ParseTopoFile(data)
	if err != nil {
		return nil, fmt.Errorf("parse topology: %w", err)
	}
	keyed := make(map[string]string, len(files))
	for k, v := range files {
		keyed[FileKey(k)] = v
	}
	return f.resolve(&labFiles{files: keyed, store: store})
}
