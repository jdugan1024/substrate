package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type IngestClient struct {
	baseURL string
	pat     string
	http    *http.Client
}

type fatalAuthError struct{ status int }

func (e fatalAuthError) Error() string { return fmt.Sprintf("ingest auth failed: HTTP %d", e.status) }

func IsFatalAuthError(err error) bool {
	_, ok := err.(fatalAuthError)
	return ok
}

func NewIngestClient(baseURL, pat string, hc *http.Client) *IngestClient {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &IngestClient{baseURL: strings.TrimRight(baseURL, "/"), pat: pat, http: hc}
}

func (c *IngestClient) Post(ctx context.Context, batch IngestBatch) (IngestResult, error) {
	var result IngestResult
	body, err := json.Marshal(batch)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ingest", bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	req.Header.Set("Authorization", "Bearer "+c.pat)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		io.Copy(io.Discard, resp.Body)
		return result, fatalAuthError{status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return result, fmt.Errorf("ingest failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return result, err
	}
	return result, nil
}
