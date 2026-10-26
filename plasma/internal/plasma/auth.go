package plasma

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The network API is TLS with one bearer token per engine (phase 1: one user
// or CI job per engine). Both are generated on first start, so an engine is
// secure without any setup; `plasmad --print-login` prints what a client
// needs. The certificate is self-signed and clients pin its fingerprint, so
// no CA is involved — a real certificate can be dropped in at the same paths.

const (
	TLSDir    = "/var/lib/plasma/tls"
	TLSCert   = "/var/lib/plasma/tls/cert.pem"
	TLSKey    = "/var/lib/plasma/tls/key.pem"
	TokenFile = "/var/lib/plasma/token"
	// DefaultListen is the API port when --listen gives only a host.
	DefaultPort = "7443"
)

// EnsureToken reads the engine's token, generating one if there is none.
func EnsureToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// EnsureCert loads the engine's certificate, generating a self-signed one
// for this host's names and addresses if there is none.
func EnsureCert(certPath, keyPath string) (tls.Certificate, error) {
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		if err := generateCert(certPath, keyPath); err != nil {
			return tls.Certificate{}, fmt.Errorf("generate certificate: %w", err)
		}
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

func generateCert(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "plasmad " + host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	if host != "" {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	tmpl.IPAddresses = append(tmpl.IPAddresses, HostAddresses()...)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// localOnlyIfaces are host-internal networks (plasma's, Docker's, libvirt's,
// CNI's): their addresses mean nothing to another host, so a client must
// never be pointed at one.
var localOnlyIfaces = []string{"plasma-br", "docker", "br-", "veth", "virbr", "cni", "flannel", "vxlan", "lxc", "lxd",
	"tap-", "pt", LinkBridgePrefix, NodeBridgePrefix}

func localOnlyIface(name string) bool {
	for _, p := range localOnlyIfaces {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// HostAddresses are the addresses other hosts can reach this one on: unicast,
// not loopback, not a host-internal bridge. The default route's interface
// comes first, as the likeliest one a client can reach.
func HostAddresses() []net.IP {
	def := defaultRouteIface()
	var first, rest []net.IP
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || localOnlyIface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || !ipn.IP.IsGlobalUnicast() {
				continue
			}
			if ifc.Name == def {
				first = append(first, ipn.IP)
			} else {
				rest = append(rest, ipn.IP)
			}
		}
	}
	return append(first, rest...)
}

// defaultRouteIface names the interface of the IPv4 default route.
func defaultRouteIface() string {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 2 && f[1] == "00000000" {
			return f[0]
		}
	}
	return ""
}

// Fingerprint is the SHA-256 of a certificate's DER, as clients pin it.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// NormalizeFingerprint accepts sha256:<hex>, bare hex, or openssl's
// colon-separated uppercase form.
func NormalizeFingerprint(fp string) (string, error) {
	f := strings.ToLower(strings.TrimSpace(fp))
	f = strings.TrimPrefix(f, "sha256:")
	f = strings.TrimPrefix(f, "sha256 fingerprint=")
	f = strings.ReplaceAll(f, ":", "")
	if !hexDigest.MatchString(f) {
		return "", fmt.Errorf("fingerprint %q: want sha256:<64 hex digits>", fp)
	}
	return "sha256:" + f, nil
}

// requireToken guards the network listener. The Unix socket is not wrapped:
// access to it is already limited to the plasma group by file mode.
func requireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="plasma"`)
			http.Error(w, "missing or wrong engine token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), networkCaller{}, true)))
	})
}

type networkCaller struct{}

// fromNetwork reports a request that came in over the network listener
// rather than the plasma-group Unix socket.
func fromNetwork(r *http.Request) bool {
	v, _ := r.Context().Value(networkCaller{}).(bool)
	return v
}

// ServerTLSConfig is HTTP/1.1 only: tunnels and the console hijack the
// connection, which HTTP/2 does not allow.
func ServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	}
}
