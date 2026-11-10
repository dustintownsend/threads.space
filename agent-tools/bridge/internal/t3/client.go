package t3

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Client owns one Streamable HTTP MCP session.
type Client struct {
	Origin          string
	CredentialsFile string
	sessionID       string
	protocol        string
	sequence        atomic.Int64
	http            *http.Client
}

func New(ctx context.Context, origin, credentialsFile string) (*Client, error) {
	origin, err := ValidateOrigin(origin)
	if err != nil {
		return nil, err
	}
	c := &Client{Origin: origin, CredentialsFile: credentialsFile, protocol: "2025-03-26", http: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.rpc(ctx, "initialize", map[string]any{"protocolVersion": c.protocol, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "threads-agent-bridge", "version": "0.1.0"}}, &initialized); err != nil {
		return nil, err
	}
	if initialized.ProtocolVersion != "" {
		c.protocol = initialized.ProtocolVersion
	}
	if err := c.rpc(ctx, "notifications/initialized", nil, nil); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) Close() {
	if c.sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.Origin+"/mcp", nil)
	if err != nil {
		return
	}
	token, err := accessToken(ctx, c.Origin, c.CredentialsFile)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Mcp-Session-Id", c.sessionID)
	req.Header.Set("MCP-Protocol-Version", c.protocol)
	if resp, err := c.http.Do(req); err == nil {
		resp.Body.Close()
	}
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *Client) rpc(ctx context.Context, method string, params, out any) error {
	payload := map[string]any{"jsonrpc": "2.0", "method": method}
	id := c.sequence.Add(1)
	if out != nil {
		payload["id"] = id
	}
	if params != nil {
		payload["params"] = params
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	token, err := accessToken(ctx, c.Origin, c.CredentialsFile)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Origin+"/mcp", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", c.protocol)
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("T3 MCP %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("T3 MCP %s returned HTTP %d (for 401, run -t3-setup)", method, resp.StatusCode)
	}
	if session := resp.Header.Get("Mcp-Session-Id"); session != "" {
		c.sessionID = session
	}
	if out == nil {
		return nil
	}
	var response rpcResponse
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		scanner := bufio.NewScanner(io.LimitReader(resp.Body, 32<<20))
		scanner.Buffer(make([]byte, 64*1024), 16<<20)
		var data strings.Builder
		found := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				data.WriteByte('\n')
			}
			if line != "" || data.Len() == 0 {
				continue
			}
			if err := json.Unmarshal([]byte(data.String()), &response); err != nil {
				return fmt.Errorf("invalid T3 MCP event: %w", err)
			}
			data.Reset()
			if string(response.ID) == fmt.Sprint(id) {
				found = true
				break
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("T3 MCP stream ended without a matching response")
		}
	} else {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&response); err != nil {
			return fmt.Errorf("invalid T3 MCP response: %w", err)
		}
	}
	if string(response.ID) != fmt.Sprint(id) {
		return fmt.Errorf("T3 MCP response id mismatch")
	}
	if response.Error != nil {
		return fmt.Errorf("T3 MCP %s: %s", method, response.Error.Message)
	}
	return json.Unmarshal(response.Result, out)
}

func (c *Client) Call(ctx context.Context, name string, args, out any) error {
	var result struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &result); err != nil {
		return err
	}
	if result.IsError {
		message := "tool failed"
		for _, item := range result.Content {
			if item.Type == "text" {
				message = item.Text
				break
			}
		}
		return fmt.Errorf("T3 %s: %s", name, message)
	}
	data := result.StructuredContent
	if len(data) == 0 || string(data) == "null" {
		for _, item := range result.Content {
			if item.Type == "text" {
				data = []byte(item.Text)
				break
			}
		}
	}
	if len(data) == 0 {
		return fmt.Errorf("T3 %s returned no structured result", name)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid T3 %s result: %w", name, err)
	}
	return nil
}

func (c *Client) Check(ctx context.Context) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.Call(ctx, "t3_project_list", map[string]any{}, &result)
	return result, err
}

type Acceptance struct {
	ThreadID  string `json:"threadId"`
	ProjectID string `json:"projectId"`
	RunID     string `json:"runId"`
	Status    string `json:"status"`
	Delivery  string `json:"delivery"`
}

type Item struct {
	Position       int    `json:"position"`
	Visibility     string `json:"visibility"`
	SourceThreadID string `json:"sourceThreadId"`
	ItemID         string `json:"itemId"`
	RunID          string `json:"runId"`
	CreatedBy      string `json:"createdBy"`
	Type           string `json:"type"`
	Status         string `json:"status"`
	Title          string `json:"title"`
	Text           string `json:"text"`
	TextTruncated  bool   `json:"textTruncated"`
	NextTextOffset *int   `json:"nextTextOffset"`
	UpdatedAt      string `json:"updatedAt"`
}

type ReadResult struct {
	Thread struct {
		ThreadID            string  `json:"threadId"`
		ProjectID           string  `json:"projectId"`
		Link                string  `json:"link"`
		Settled             bool    `json:"settled"`
		ParentThreadID      *string `json:"parentThreadId"`
		ActiveRunID         string  `json:"activeRunId"`
		PendingRequestCount int     `json:"pendingRequestCount"`
	} `json:"thread"`
	RecentRuns []struct {
		RunID  string `json:"runId"`
		Status string `json:"status"`
	} `json:"recentRuns"`
	Items        []Item `json:"items"`
	HasMore      bool   `json:"hasMore"`
	NextPosition *int   `json:"nextPosition"`
}
