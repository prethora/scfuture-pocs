package coordinator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"scfuture/internal/shared"
)

type MachineClient struct {
	address string
	client  *http.Client
}

func NewMachineClient(address string) *MachineClient {
	return &MachineClient{
		address: address,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *MachineClient) CreateImage(userID string, sizeMB int) (*shared.ImageCreateResponse, error) {
	req := shared.ImageCreateRequest{ImageSizeMB: sizeMB}
	var resp shared.ImageCreateResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/create", userID), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) DRBDCreate(userID string, req *shared.DRBDCreateRequest) (*shared.DRBDCreateResponse, error) {
	var resp shared.DRBDCreateResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/drbd/create", userID), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) DRBDPromote(userID string) (*shared.DRBDPromoteResponse, error) {
	var resp shared.DRBDPromoteResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/drbd/promote", userID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) DRBDDemote(userID string) (*shared.DRBDDemoteResponse, error) {
	var resp shared.DRBDDemoteResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/drbd/demote", userID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) DRBDStatus(userID string) (*shared.DRBDStatusResponse, error) {
	var resp shared.DRBDStatusResponse
	if err := c.doJSON("GET", fmt.Sprintf("/images/%s/drbd/status", userID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) FormatBtrfs(userID string) (*shared.FormatBtrfsResponse, error) {
	var resp shared.FormatBtrfsResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/format-btrfs", userID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) ContainerStart(userID string) (*shared.ContainerStartResponse, error) {
	var resp shared.ContainerStartResponse
	if err := c.doJSON("POST", fmt.Sprintf("/containers/%s/start", userID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) ContainerStop(userID string) error {
	return c.doJSON("POST", fmt.Sprintf("/containers/%s/stop", userID), nil, nil)
}

func (c *MachineClient) ContainerStatus(userID string) (*shared.ContainerStatusResponse, error) {
	var resp shared.ContainerStatusResponse
	if err := c.doJSON("GET", fmt.Sprintf("/containers/%s/status", userID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) DRBDDisconnect(userID string) (*shared.DRBDDisconnectResponse, error) {
	var resp shared.DRBDDisconnectResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/drbd/disconnect", userID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) DRBDReconfigure(userID string, req *shared.DRBDReconfigureRequest) (*shared.DRBDReconfigureResponse, error) {
	var resp shared.DRBDReconfigureResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/drbd/reconfigure", userID), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) DRBDDestroy(userID string) error {
	return c.doJSON("DELETE", fmt.Sprintf("/images/%s/drbd", userID), nil, nil)
}

func (c *MachineClient) DeleteUser(userID string) error {
	return c.doJSON("DELETE", fmt.Sprintf("/images/%s", userID), nil, nil)
}

func (c *MachineClient) Status() (*shared.StatusResponse, error) {
	var resp shared.StatusResponse
	if err := c.doJSON("GET", "/status", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) Cleanup() error {
	return c.doJSON("POST", "/cleanup", nil, nil)
}

func (c *MachineClient) Snapshot(userID string, req *shared.SnapshotRequest) (*shared.SnapshotResponse, error) {
	var resp shared.SnapshotResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/snapshot", userID), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) Backup(userID string, req *shared.BackupRequest) (*shared.BackupResponse, error) {
	// Backup can be slow — use a client with longer timeout
	longClient := &MachineClient{
		address: c.address,
		client:  &http.Client{Timeout: 300 * time.Second},
	}
	var resp shared.BackupResponse
	if err := longClient.doJSON("POST", fmt.Sprintf("/images/%s/backup", userID), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) Restore(userID string, req *shared.RestoreRequest) (*shared.RestoreResponse, error) {
	// Restore can be slow — use a client with longer timeout
	longClient := &MachineClient{
		address: c.address,
		client:  &http.Client{Timeout: 300 * time.Second},
	}
	var resp shared.RestoreResponse
	if err := longClient.doJSON("POST", fmt.Sprintf("/images/%s/restore", userID), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) BackupStatus(userID string) (*shared.BackupStatusResponse, error) {
	var resp shared.BackupStatusResponse
	if err := c.doJSON("GET", fmt.Sprintf("/images/%s/backup/status", userID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) DRBDConnect(userID string) (*shared.DRBDConnectResponse, error) {
	var resp shared.DRBDConnectResponse
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/drbd/connect", userID), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) FormatBtrfsBare(userID string) (*shared.FormatBtrfsResponse, error) {
	var resp shared.FormatBtrfsResponse
	req := shared.FormatBtrfsRequest{Bare: true}
	if err := c.doJSON("POST", fmt.Sprintf("/images/%s/format-btrfs", userID), req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *MachineClient) doJSON(method, path string, reqBody interface{}, respBody interface{}) error {
	url := "http://" + c.address + path

	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s %s failed: %w", method, url, err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned %d: %s", method, url, resp.StatusCode, string(respData))
	}

	if respBody != nil && len(respData) > 0 {
		if err := json.Unmarshal(respData, respBody); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}

	return nil
}
