package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Client struct {
	BaseURL, Token string
	HTTP           *http.Client
}

func (c *Client) UploadArtifact(path, filePath string) (int, error) {
	return c.UploadArtifactContext(context.Background(), path, filePath)
}

func (c *Client) UploadArtifactContext(ctx context.Context, path, filePath string) (int, error) {
	return c.UploadArtifactContextWithHeaders(ctx, path, filePath, nil)
}

func (c *Client) UploadArtifactWithHeaders(path, filePath string, headers http.Header) (int, error) {
	return c.UploadArtifactContextWithHeaders(context.Background(), path, filePath, headers)
}

func (c *Client) UploadArtifactContextWithHeaders(ctx context.Context, path, filePath string, headers http.Header) (int, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	pipeReader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)
	go func() {
		part, createErr := writer.CreateFormFile("file", filepath.Base(filePath))
		if createErr == nil {
			_, createErr = io.Copy(part, file)
		}
		if closeErr := writer.Close(); createErr == nil {
			createErr = closeErr
		}
		_ = pipeWriter.CloseWithError(createErr)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, pipeReader)
	if err != nil {
		return 0, err
	}
	copyHeaders(req.Header, headers)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := *c.HTTP
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return resp.StatusCode, nil
}

func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) Do(method, path string, body, out any) (int, error) {
	return c.DoContext(context.Background(), method, path, body, out)
}

func (c *Client) DoContext(ctx context.Context, method, path string, body, out any) (int, error) {
	return c.DoContextWithHeaders(ctx, method, path, body, out, nil)
}

func (c *Client) DoWithHeaders(method, path string, body, out any, headers http.Header) (int, error) {
	return c.DoContextWithHeaders(context.Background(), method, path, body, out, headers)
}

func (c *Client) DoContextWithHeaders(ctx context.Context, method, path string, body, out any, headers http.Header) (int, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return 0, err
	}
	copyHeaders(req.Header, headers)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}
