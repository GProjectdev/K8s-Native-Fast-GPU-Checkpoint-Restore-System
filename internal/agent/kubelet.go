package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// KubeletClient calls the kubelet's container-checkpoint API.
//
// The kubelet exposes (alpha feature gate ContainerCheckpoint, GA-track since
// v1.30) an authenticated endpoint on its secure port:
//
//	POST https://<node>:10250/checkpoint/{namespace}/{pod}/{container}
//
// It drives the CRI ContainerCheckpoint call (CRI-O / containerd) which uses
// CRIU to snapshot the container's CPU-side state and writes a tar archive
// (default dir: /var/lib/kubelet/checkpoints). The JSON response lists the
// produced archive path(s).
type KubeletClient struct {
	// BaseURL e.g. https://127.0.0.1:10250 (agent runs hostNetwork on the node).
	BaseURL string
	// Token is the bearer token used to authenticate to the kubelet API
	// (the agent's ServiceAccount token, granted nodes/checkpoint via RBAC).
	Token string
	http  *http.Client
}

// checkpointResponse mirrors the kubelet /checkpoint reply.
type checkpointResponse struct {
	Items []string `json:"items"`
}

// NewKubeletClient builds a client. If insecure is true the kubelet's serving
// certificate is not verified (common for self-signed kubelet certs); otherwise
// caFile must point at the kubelet CA bundle.
func NewKubeletClient(baseURL, token, caFile string, insecure bool, timeout time.Duration) (*KubeletClient, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // configurable
	if !insecure && caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read kubelet CA %s: %w", caFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no valid certs in kubelet CA %s", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &KubeletClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		http: &http.Client{
			Timeout:   timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// Checkpoint requests a checkpoint of the given container and returns the paths
// of the produced archive(s) on the node filesystem.
func (k *KubeletClient) Checkpoint(ctx context.Context, namespace, pod, container string) ([]string, error) {
	url := fmt.Sprintf("%s/checkpoint/%s/%s/%s", k.BaseURL, namespace, pod, container)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	if k.Token != "" {
		req.Header.Set("Authorization", "Bearer "+k.Token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := k.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kubelet checkpoint call: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kubelet checkpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var cr checkpointResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, fmt.Errorf("decode kubelet response %q: %w", string(body), err)
	}
	if len(cr.Items) == 0 {
		return nil, fmt.Errorf("kubelet returned no checkpoint archive")
	}
	return cr.Items, nil
}
