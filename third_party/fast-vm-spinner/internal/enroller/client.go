package enroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Client struct {
	BaseURL    string
	Token      string
	TokenFile  string
	HTTPClient *http.Client
}

func (c *Client) Enroll(ctx context.Context, req EnrollRequest) error {
	if strings.TrimSpace(c.BaseURL) == "" {
		return fmt.Errorf("enroller base URL is empty")
	}
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil {
		return fmt.Errorf("parse enroller URL: %w", err)
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/v1/enroll"

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal enrollment request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build enrollment request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(c.authToken()); token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("call enroller: %w", err)
	}
	defer resp.Body.Close()

	var decoded struct {
		OK    bool    `json:"ok"`
		Error *string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return fmt.Errorf("decode enroller response status %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !decoded.OK {
		if decoded.Error != nil && *decoded.Error != "" {
			return fmt.Errorf("enroller returned status %d: %s", resp.StatusCode, *decoded.Error)
		}
		return fmt.Errorf("enroller returned status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) authToken() string {
	if strings.TrimSpace(c.Token) != "" {
		return c.Token
	}
	if strings.TrimSpace(c.TokenFile) == "" {
		return ""
	}
	data, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
