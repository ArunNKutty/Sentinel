package plasma

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

type Client struct {
	http *http.Client
}

func NewClient(socket string) *Client {
	return &Client{
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

func (c *Client) Launch(group string) (VmRecord, error) {
	body, _ := json.Marshal(LaunchRequest{Group: group})
	resp, err := c.http.Post("http://plasma/v1/vms", "application/json", bytes.NewReader(body))
	if err != nil {
		return VmRecord{}, err
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

func (c *Client) List() ([]VmRecord, error) {
	resp, err := c.http.Get("http://plasma/v1/vms")
	if err != nil {
		return nil, err
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
