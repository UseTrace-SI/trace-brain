// Package semantic implements explicit local model operations. Ordinary recall
// and context remain offline database reads and never start or contact a model.
package semantic

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Ollama struct {
	origin string
	client *http.Client
}

func NewOllama(endpoint string) (*Ollama, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid model endpoint")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("model endpoint must be an HTTP(S) loopback origin without credentials, path or query")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("model endpoint must use localhost or a loopback IP")
	}
	transport := &http.Transport{Proxy: nil, MaxIdleConns: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var last error
		for _, address := range addresses {
			if !address.IP.IsLoopback() {
				return nil, fmt.Errorf("model endpoint resolved outside loopback")
			}
			conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, net.JoinHostPort(address.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		if last == nil {
			last = fmt.Errorf("model endpoint has no loopback address")
		}
		return nil, last
	}
	return &Ollama{origin: strings.TrimSuffix(u.String(), "/"), client: &http.Client{Transport: transport, Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("model redirects are not allowed") }}}, nil
}

func (o *Ollama) Origin() string { return o.origin }

func (o *Ollama) Close() { o.client.CloseIdleConnections() }

func (o *Ollama) request(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, o.origin+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("local model request failed: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("local model returned HTTP %d", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return fmt.Errorf("model response exceeds 8 MiB")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid model response")
	}
	return nil
}

func (o *Ollama) Digest(ctx context.Context, model string) (string, error) {
	if strings.TrimSpace(model) == "" || len(model) > 256 {
		return "", fmt.Errorf("an exact installed model name of 1–256 bytes is required")
	}
	var out struct {
		Models []struct {
			Name   string `json:"name"`
			Model  string `json:"model"`
			Digest string `json:"digest"`
		} `json:"models"`
	}
	if err := o.request(ctx, http.MethodGet, "/api/tags", nil, &out); err != nil {
		return "", err
	}
	for _, m := range out.Models {
		if m.Name == model || m.Model == model {
			digest := strings.TrimPrefix(m.Digest, "sha256:")
			if len(digest) != 64 {
				return "", fmt.Errorf("model digest is missing or invalid")
			}
			if _, err := hex.DecodeString(digest); err != nil {
				return "", fmt.Errorf("invalid model digest")
			}
			return strings.ToLower(digest), nil
		}
	}
	return "", fmt.Errorf("model is not installed under that exact name; Trace never pulls models automatically")
}

func (o *Ollama) Embed(ctx context.Context, model string, texts []string) ([][]float32, error) {
	if len(texts) == 0 || len(texts) > 16 {
		return nil, fmt.Errorf("embedding batches must contain 1–16 texts")
	}
	for _, text := range texts {
		if len(text) > 65536 {
			return nil, fmt.Errorf("embedding input exceeds 64 KiB")
		}
	}
	body, err := json.Marshal(map[string]any{"model": model, "input": texts, "truncate": false})
	if err != nil {
		return nil, err
	}
	var out struct {
		Model      string      `json:"model"`
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := o.request(ctx, http.MethodPost, "/api/embed", body, &out); err != nil {
		return nil, err
	}
	if out.Model != model || len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("model identity or embedding count mismatch")
	}
	dim := len(out.Embeddings[0])
	if dim < 1 || dim > 8192 {
		return nil, fmt.Errorf("embedding dimension must be 1–8192")
	}
	for _, vector := range out.Embeddings {
		if len(vector) != dim {
			return nil, fmt.Errorf("inconsistent embedding dimensions")
		}
		var norm float64
		for _, v := range vector {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return nil, fmt.Errorf("non-finite embedding")
			}
			norm += float64(v) * float64(v)
		}
		if norm == 0 {
			return nil, fmt.Errorf("model returned a zero embedding")
		}
	}
	return out.Embeddings, nil
}
