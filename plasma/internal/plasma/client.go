package plasma

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
)

type Client struct {
	http   *http.Client
	socket string
}

func NewClient(socket string) *Client {
	return &Client{
		socket: socket,
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

// dialErr turns the transport's errors into something a CLI user can act on.
// The two that actually happen are "daemon isn't running" and "you aren't in
// the plasma group".
func (c *Client) dialErr(err error) error {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("no access to %s: add yourself to the %q group (sudo usermod -aG %s $USER, then re-login)",
			c.socket, SocketGroup, SocketGroup)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("plasmad is not running (no listener on %s)", c.socket)
	}
	return err
}

func (c *Client) Launch(group string) (VmRecord, error) {
	return c.LaunchSpec(LaunchRequest{Group: group})
}

// SetLinks records the links declared by an applied topology file.
func (c *Client) SetLinks(doc LinksDocument) error {
	body, _ := json.Marshal(doc)
	req, err := http.NewRequest(http.MethodPut, "http://plasma/v1/topology/links", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (c *Client) LaunchSpec(spec LaunchRequest) (VmRecord, error) {
	body, _ := json.Marshal(spec)
	resp, err := c.http.Post("http://plasma/v1/vms", "application/json", bytes.NewReader(body))
	if err != nil {
		return VmRecord{}, c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return VmRecord{}, fmt.Errorf("daemon: %s: %s", resp.Status, msg)
	}
	var rec VmRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return VmRecord{}, err
	}
	return rec, nil
}

func (c *Client) Delete(id string) error {
	req, err := http.NewRequest(http.MethodDelete, "http://plasma/v1/vms/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon: %s: %s", resp.Status, msg)
	}
	return nil
}

// Capture opens a live pcap stream for a VM (or CaptureAllTarget for the
// bridge). The caller must Close the reader to stop the capture on the daemon
// side. Cancelling ctx also stops it.
func (c *Client) Capture(ctx context.Context, id string, opts CaptureOptions) (io.ReadCloser, error) {
	q := url.Values{}
	if opts.Filter != "" {
		q.Set("filter", opts.Filter)
	}
	if opts.Snaplen > 0 {
		q.Set("snaplen", strconv.Itoa(opts.Snaplen))
	}

	endpoint := "http://plasma/v1/vms/" + url.PathEscape(id) + "/capture"
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.dialErr(err)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("daemon: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp.Body, nil
}

func (c *Client) Topology() (Topology, error) {
	resp, err := c.http.Get("http://plasma/v1/topology")
	if err != nil {
		return Topology{}, c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return Topology{}, fmt.Errorf("daemon: %s: %s", resp.Status, msg)
	}
	var t Topology
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return Topology{}, err
	}
	return t, nil
}

func (c *Client) List() ([]VmRecord, error) {
	resp, err := c.http.Get("http://plasma/v1/vms")
	if err != nil {
		return nil, c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("daemon: %s: %s", resp.Status, msg)
	}
	var lr ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, err
	}
	return lr.VMs, nil
}
