package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

// HTTPControl is the real Control implementation: it talks to the coordinator
// over HTTP/JSON. It is the agent's only outbound dependency, so the agent
// stays pull-only (it initiates every call; the coordinator never dials back).
type HTTPControl struct {
	// BaseURL is the coordinator root, e.g. "http://office-coord.tailnet:7070".
	BaseURL string
	Client  *http.Client
}

// NewHTTPControl builds a client against baseURL (trailing slash trimmed).
func NewHTTPControl(baseURL string) *HTTPControl {
	return &HTTPControl{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{},
	}
}

func (h *HTTPControl) Register(ctx context.Context, node types.Node) error {
	return h.postJSON(ctx, "/register", node, nil)
}

func (h *HTTPControl) Heartbeat(ctx context.Context, nodeID string) error {
	return h.postJSON(ctx, "/heartbeat", map[string]string{"node_id": nodeID}, nil)
}

func (h *HTTPControl) PollAssigned(ctx context.Context, nodeID string) ([]types.Job, error) {
	var jobs []types.Job
	err := h.getJSON(ctx, "/agents/"+nodeID+"/assigned", &jobs)
	return jobs, err
}

func (h *HTTPControl) PollCancels(ctx context.Context, nodeID string) ([]string, error) {
	var ids []string
	err := h.getJSON(ctx, "/agents/"+nodeID+"/cancels", &ids)
	return ids, err
}

func (h *HTTPControl) ReportStatus(ctx context.Context, jobID string, state types.JobState, exitCode int) error {
	body := map[string]any{"state": state, "exit_code": exitCode}
	return h.postJSON(ctx, "/jobs/"+jobID+"/status", body, nil)
}

func (h *HTTPControl) AppendLogs(ctx context.Context, jobID string, chunks []LogChunk) error {
	return h.postJSON(ctx, "/jobs/"+jobID+"/logs", chunks, nil)
}

func (h *HTTPControl) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.BaseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusErr(path, resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (h *HTTPControl) postJSON(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusErr(path, resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func statusErr(path string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	return fmt.Errorf("%s: coordinator returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
}
