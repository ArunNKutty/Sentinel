package plasma

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Client talks to an engine: the local daemon over its Unix socket, or a
// remote one over TLS with a token. Every call is the same either way.
type Client struct {
	http *http.Client
	base string
	// socket is set for a local engine, url for a remote one; both only
	// shape error messages.
	socket string
	remote *EngineConfig
}

// EngineConfig is how a client reaches a remote engine.
type EngineConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	// Fingerprint pins the engine's certificate (sha256:<hex>). Without it,
	// CAFile or the system roots must vouch for the certificate.
	Fingerprint string `json:"fingerprint,omitempty"`
	CAFile      string `json:"ca_file,omitempty"`
}

func NewClient(socket string) *Client {
	return &Client{
		socket: socket,
		base:   "http://plasma",
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

// NewRemoteClient connects to an engine's network API.
func NewRemoteClient(cfg EngineConfig) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("engine %q: want https://host[:port]", cfg.URL)
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), DefaultPort)
	}
	cfg.URL = u.String()
	if cfg.Token == "" {
		return nil, fmt.Errorf("engine %s: no token (PLASMA_TOKEN, or plasma engine login)", cfg.URL)
	}
	tlsCfg, err := clientTLS(cfg)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     tlsCfg,
		TLSHandshakeTimeout: 15 * time.Second,
		// Streams hijack the connection, which needs HTTP/1.1.
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &Client{
		base:   cfg.URL,
		remote: &cfg,
		http:   &http.Client{Transport: &bearer{token: cfg.Token, next: tr}},
	}, nil
}

func clientTLS(cfg EngineConfig) (*tls.Config, error) {
	t := &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	switch {
	case cfg.Fingerprint != "":
		want, err := NormalizeFingerprint(cfg.Fingerprint)
		if err != nil {
			return nil, err
		}
		// The pin is the trust: the certificate is self-signed, so chain
		// and name checks are replaced by an exact match on its hash.
		t.InsecureSkipVerify = true
		t.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("engine sent no certificate")
			}
			if got := Fingerprint(cs.PeerCertificates[0].Raw); got != want {
				return fmt.Errorf("engine certificate is %s, but %s is pinned: "+
					"the engine was reinstalled, or this is not the engine", got, want)
			}
			return nil
		}
	case cfg.CAFile != "":
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no certificate", cfg.CAFile)
		}
		t.RootCAs = pool
	}
	return t, nil
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b *bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// ConfigPath is where `plasma engine login` keeps the remote engine.
func ConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "plasma", "engine.json"), nil
}

func LoadEngineConfig() (*EngineConfig, error) {
	p, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c EngineConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &c, nil
}

func SaveEngineConfig(c EngineConfig) (string, error) {
	p, err := ConfigPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	// The token is a credential for the whole engine.
	return p, os.WriteFile(p, data, 0o600)
}

// ClientFromEnv picks the engine: PLASMA_ENGINE (an https URL, with
// PLASMA_TOKEN and PLASMA_FINGERPRINT or PLASMA_CA; or "local"), else the
// engine saved by `plasma engine login`, else the local daemon.
func ClientFromEnv() (*Client, error) {
	switch e := os.Getenv("PLASMA_ENGINE"); e {
	case "local":
		return NewClient(SocketPath), nil
	case "":
	default:
		return NewRemoteClient(EngineConfig{
			URL: e, Token: os.Getenv("PLASMA_TOKEN"),
			Fingerprint: os.Getenv("PLASMA_FINGERPRINT"), CAFile: os.Getenv("PLASMA_CA"),
		})
	}
	cfg, err := LoadEngineConfig()
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		return NewRemoteClient(*cfg)
	}
	return NewClient(SocketPath), nil
}

// Remote reports whether this client talks to a remote engine.
func (c *Client) Remote() bool { return c.remote != nil }

// Where names the engine for messages.
func (c *Client) Where() string {
	if c.remote != nil {
		return c.remote.URL
	}
	return c.socket
}

// FetchFingerprint connects to an engine without trusting it and returns its
// certificate's fingerprint, for a user to confirm on first login.
func FetchFingerprint(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("engine %q: want https://host[:port]", rawURL)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), DefaultPort)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", host,
		&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return Fingerprint(conn.ConnectionState().PeerCertificates[0].Raw), nil
}

// dialErr turns the transport's errors into something a CLI user can act on.
// Locally the two that actually happen are "daemon isn't running" and "you
// aren't in the plasma group".
func (c *Client) dialErr(err error) error {
	if c.remote != nil {
		return fmt.Errorf("engine %s: %w", c.remote.URL, err)
	}
	switch {
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("no access to %s: add yourself to the %q group (sudo usermod -aG %s $USER, then re-login)",
			c.socket, SocketGroup, SocketGroup)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("plasmad is not running (no listener on %s)", c.socket)
	}
	return err
}

func statusErr(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	text := strings.TrimSpace(string(msg))
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("engine rejected the token: %s", text)
	}
	return &APIError{Status: resp.StatusCode, Message: text}
}

// APIError is a non-success answer from the engine.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("daemon: %d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

// do sends a request with an optional JSON body and decodes a JSON answer
// into out (when non-nil). Any status in ok counts as success.
func (c *Client) do(ctx context.Context, method, path string, body, out any, ok ...int) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.dialErr(err)
	}
	defer resp.Body.Close()
	good := false
	for _, s := range ok {
		good = good || resp.StatusCode == s
	}
	if !good {
		return statusErr(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Launch(group string) (VmRecord, error) {
	return c.LaunchSpec(LaunchRequest{Group: group})
}

// SetLinks records the links declared by an applied topology file.
func (c *Client) SetLinks(doc LinksDocument) error {
	return c.do(context.Background(), http.MethodPut, "/v1/topology/links", doc, nil, http.StatusNoContent)
}

func (c *Client) LaunchSpec(spec LaunchRequest) (VmRecord, error) {
	var rec VmRecord
	err := c.do(context.Background(), http.MethodPost, "/v1/vms", spec, &rec, http.StatusOK)
	return rec, err
}

func (c *Client) Delete(id string) error {
	return c.do(context.Background(), http.MethodDelete, "/v1/vms/"+url.PathEscape(id), nil, nil, http.StatusNoContent)
}

func captureQuery(q url.Values, opts CaptureOptions) string {
	if opts.Filter != "" {
		q.Set("filter", opts.Filter)
	}
	if opts.Snaplen > 0 {
		q.Set("snaplen", strconv.Itoa(opts.Snaplen))
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// Capture opens a live pcap stream for a legacy VM (or CaptureAllTarget for
// the bridge). The caller must Close the reader to stop the capture on the
// daemon side. Cancelling ctx also stops it.
func (c *Client) Capture(ctx context.Context, id string, opts CaptureOptions) (io.ReadCloser, error) {
	return c.stream(ctx, "/v1/vms/"+url.PathEscape(id)+"/capture"+captureQuery(url.Values{}, opts))
}

// LabCapture opens a live pcap stream on a lab target: mgmt, <vm>,
// <vm>:<ethN> or a bridge node's name.
func (c *Client) LabCapture(ctx context.Context, lab, target string, opts CaptureOptions) (io.ReadCloser, error) {
	q := url.Values{"target": {target}}
	return c.stream(ctx, "/v1/labs/"+url.PathEscape(lab)+"/capture"+captureQuery(q, opts))
}

func (c *Client) stream(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.dialErr(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, statusErr(resp)
	}
	return resp.Body, nil
}

func (c *Client) Topology() (Topology, error) {
	var t Topology
	err := c.do(context.Background(), http.MethodGet, "/v1/topology", nil, &t, http.StatusOK)
	return t, err
}

func (c *Client) List() ([]VmRecord, error) {
	var lr ListResponse
	err := c.do(context.Background(), http.MethodGet, "/v1/vms", nil, &lr, http.StatusOK)
	return lr.VMs, err
}

func (c *Client) Engine(ctx context.Context) (EngineInfo, error) {
	var info EngineInfo
	err := c.do(ctx, http.MethodGet, "/v1/engine", nil, &info, http.StatusOK)
	return info, err
}

// HashFile is a file's artifact digest.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// PushArtifact uploads a file unless the engine already holds it. Reports
// the digest and whether bytes were sent.
func (c *Client) PushArtifact(ctx context.Context, path string) (digest string, uploaded bool, err error) {
	digest, err = HashFile(path)
	if err != nil {
		return "", false, err
	}
	endpoint := c.base + "/v1/artifacts/" + digest
	head, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return "", false, err
	}
	resp, err := c.http.Do(head)
	if err != nil {
		return "", false, c.dialErr(err)
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return digest, false, nil
	case http.StatusNotFound:
	default:
		return "", false, fmt.Errorf("artifact %s: engine answered %s", digest, resp.Status)
	}

	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, f)
	if err != nil {
		return "", false, err
	}
	put.ContentLength = st.Size()
	put.Header.Set("Content-Type", "application/octet-stream")
	resp, err = c.http.Do(put)
	if err != nil {
		return "", false, c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", false, statusErr(resp)
	}
	return digest, resp.StatusCode == http.StatusCreated, nil
}

func (c *Client) CreateLab(ctx context.Context, req CreateLabRequest) (LabStatus, error) {
	var st LabStatus
	err := c.do(ctx, http.MethodPost, "/v1/labs", req, &st, http.StatusAccepted)
	return st, err
}

func (c *Client) Lab(ctx context.Context, lab string) (LabStatus, error) {
	var st LabStatus
	err := c.do(ctx, http.MethodGet, "/v1/labs/"+url.PathEscape(lab), nil, &st, http.StatusOK)
	return st, err
}

func (c *Client) Labs(ctx context.Context) ([]LabStatus, error) {
	var out []LabStatus
	err := c.do(ctx, http.MethodGet, "/v1/labs", nil, &out, http.StatusOK)
	return out, err
}

func (c *Client) DeleteLab(ctx context.Context, lab string) error {
	return c.do(ctx, http.MethodDelete, "/v1/labs/"+url.PathEscape(lab), nil, nil, http.StatusNoContent)
}

func (c *Client) LabTopology(ctx context.Context, lab string) (Topology, error) {
	var t Topology
	err := c.do(ctx, http.MethodGet, "/v1/labs/"+url.PathEscape(lab)+"/topology", nil, &t, http.StatusOK)
	return t, err
}

func linkPath(lab, link, op string) string {
	return "/v1/labs/" + url.PathEscape(lab) + "/links/" + url.PathEscape(link) + "/" + op
}

func (c *Client) SetFilter(ctx context.Context, lab, link string, f LinkFilter) error {
	return c.do(ctx, http.MethodPut, linkPath(lab, link, "filters"), f, nil, http.StatusNoContent)
}

func (c *Client) ClearFilter(ctx context.Context, lab, link string) error {
	return c.do(ctx, http.MethodDelete, linkPath(lab, link, "filters"), nil, nil, http.StatusNoContent)
}

func (c *Client) SetMirror(ctx context.Context, lab, link, to string) error {
	return c.do(ctx, http.MethodPut, linkPath(lab, link, "mirror"), MirrorRequest{To: to}, nil, http.StatusNoContent)
}

func (c *Client) ClearMirror(ctx context.Context, lab, link string) error {
	return c.do(ctx, http.MethodDelete, linkPath(lab, link, "mirror"), nil, nil, http.StatusNoContent)
}

// Console attaches to a VM's serial console: a lab VM, or a legacy one when
// lab is "". The caller closes the stream to detach.
func (c *Client) Console(ctx context.Context, lab, vm string) (io.ReadWriteCloser, error) {
	p := "/v1/vms/" + url.PathEscape(vm) + "/console"
	if lab != "" {
		p = "/v1/labs/" + url.PathEscape(lab) + "/vms/" + url.PathEscape(vm) + "/console"
	}
	return c.upgrade(ctx, p)
}

// Tunnel opens one TCP connection to a port on a lab VM, carried over the
// API connection.
func (c *Client) Tunnel(ctx context.Context, lab, vm string, port int) (io.ReadWriteCloser, error) {
	return c.upgrade(ctx, "/v1/labs/"+url.PathEscape(lab)+"/vms/"+url.PathEscape(vm)+"/tunnel/"+strconv.Itoa(port))
}

func (c *Client) upgrade(ctx context.Context, path string) (io.ReadWriteCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", StreamProtocol)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.dialErr(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		return nil, statusErr(resp)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, errors.New("engine did not hand over a stream")
	}
	return rwc, nil
}

func (c *Client) MirrorStatus(ctx context.Context, lab, link string) (MirrorStatus, error) {
	var st MirrorStatus
	err := c.do(ctx, http.MethodGet, linkPath(lab, link, "mirror"), nil, &st, http.StatusOK)
	return st, err
}

func (c *Client) SetDrop(ctx context.Context, lab, link string, req DropRequest) (DropStatus, error) {
	var st DropStatus
	err := c.do(ctx, http.MethodPut, linkPath(lab, link, "drop"), req, &st, http.StatusOK)
	return st, err
}

func (c *Client) DropStatus(ctx context.Context, lab, link string) (DropStatus, error) {
	var st DropStatus
	err := c.do(ctx, http.MethodGet, linkPath(lab, link, "drop"), nil, &st, http.StatusOK)
	return st, err
}

func (c *Client) ClearDrop(ctx context.Context, lab, link string) (DropStatus, error) {
	var st DropStatus
	err := c.do(ctx, http.MethodDelete, linkPath(lab, link, "drop"), nil, &st, http.StatusOK)
	return st, err
}
