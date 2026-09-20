package update

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Release mirrors the subset of the GitHub releases API response that the
// self-updater consumes.
type Release struct {
	TagName string  `json:"tag_name"`
	Name    string  `json:"name"`
	Body    string  `json:"body"`
	Assets  []Asset `json:"assets"`
}

// Asset is a single downloadable artifact attached to a release.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// CheckResult describes a trusted release check without downloading assets.
type CheckResult struct {
	Available    bool
	Applied      bool
	Current      string
	Latest       string
	ReleaseNotes string
	Release      *Release
}

const (
	githubAPI         = "https://api.github.com"
	githubProxyPrefix = "https://gh-proxy.com/"
	DefaultRepository = "Remix123/VoCatFree"
)

var githubHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	},
}

// LatestRelease fetches the newest published release for repo (form
// "owner/name"). A non-empty token is sent as a Bearer header, which is
// required for private repositories and lifts the unauthenticated rate limit.
func LatestRelease(ctx context.Context, repo, token string) (*Release, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return nil, fmt.Errorf("update: repository not configured (set --repo or VOCAT_REPO)")
	}
	if strings.Count(repo, "/") != 1 {
		return nil, fmt.Errorf("update: invalid repository %q (expected owner/name)", repo)
	}

	resp, err := githubRequest(ctx, githubAPI+"/repos/"+repo+"/releases/latest", token)
	if err != nil {
		return nil, fmt.Errorf("update: fetch latest release: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		// The releases API returns 403 (not 404) when rate-limited.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("update: GitHub API rejected the request (likely rate-limited): %s", strings.TrimSpace(string(body)))
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("update: no published release found for %s", repo)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("update: GitHub API returned %s", resp.Status)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("update: decode release JSON: %w", err)
	}
	return &release, nil
}

// CheckLatest fetches the newest release and performs a semantic version
// comparison so development builds are never offered an older release.
func CheckLatest(ctx context.Context, repo, token, current string) (CheckResult, error) {
	release, err := LatestRelease(ctx, repo, token)
	if err != nil {
		return CheckResult{}, err
	}
	latest := strings.TrimPrefix(strings.TrimSpace(release.TagName), "v")
	available, err := IsNewerVersion(current, latest)
	if err != nil {
		return CheckResult{}, fmt.Errorf("update: compare release versions: %w", err)
	}
	return CheckResult{
		Available:    available,
		Current:      current,
		Latest:       latest,
		ReleaseNotes: strings.TrimSpace(release.Body),
		Release:      release,
	}, nil
}

// downloadAsset streams a release asset into dst, honoring the request context.
// The token is applied for consistency with the API call (GitHub release assets
// redirect to a pre-signed S3 URL; the token is dropped on redirect, which is
// the expected public-CDN flow).
func downloadAsset(ctx context.Context, url, token string, dst io.Writer) error {
	attemptContext, cancel := context.WithTimeout(ctx, OperationAttemptTimeout)
	err := downloadAssetAttempt(attemptContext, url, token, dst)
	cancel()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil || !shouldTryProxy(err) {
		return err
	}
	if resetErr := resetDownloadDestination(dst); resetErr != nil {
		return fmt.Errorf("%w; update: cannot retry through accelerated URL: %v", err, resetErr)
	}

	proxyContext, cancelProxy := context.WithTimeout(ctx, OperationAttemptTimeout)
	proxyErr := downloadAssetAttempt(proxyContext, acceleratedURL(url), "", dst)
	cancelProxy()
	if proxyErr != nil {
		return fmt.Errorf("update: direct download failed: %v; accelerated download failed: %w", err, proxyErr)
	}
	return nil
}

func downloadAssetAttempt(ctx context.Context, url, token string, dst io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := githubHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("update: download asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update: asset download returned %s", resp.Status)
	}
	if _, err := io.Copy(dst, resp.Body); err != nil {
		return fmt.Errorf("update: read asset body: %w", err)
	}
	return nil
}

func githubRequest(ctx context.Context, url, token string) (*http.Response, error) {
	attemptContext, cancel := context.WithTimeout(ctx, OperationAttemptTimeout)
	resp, err := doGitHubRequest(attemptContext, url, token)
	cancel()
	if err == nil {
		return resp, nil
	}
	if ctx.Err() != nil || !shouldTryProxy(err) {
		return nil, err
	}

	proxyContext, cancelProxy := context.WithTimeout(ctx, OperationAttemptTimeout)
	proxyResp, proxyErr := doGitHubRequest(proxyContext, acceleratedURL(url), "")
	cancelProxy()
	if proxyErr != nil {
		return nil, fmt.Errorf("update: direct GitHub request failed: %v; accelerated request failed: %w", err, proxyErr)
	}
	return proxyResp, nil
}

func doGitHubRequest(ctx context.Context, url, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return githubHTTPClient.Do(req)
}

func acceleratedURL(url string) string {
	if strings.HasPrefix(url, githubProxyPrefix) {
		return url
	}
	return githubProxyPrefix + url
}

func shouldTryProxy(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errors.Is(err, context.DeadlineExceeded)
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func resetDownloadDestination(dst io.Writer) error {
	if progress, ok := dst.(*downloadProgressWriter); ok {
		progress.downloaded.Store(0)
		return resetDownloadDestination(progress.destination)
	}
	if resetter, ok := dst.(interface{ Reset() }); ok {
		resetter.Reset()
		return nil
	}
	if seeker, ok := dst.(io.Seeker); ok {
		if truncater, ok := dst.(interface{ Truncate(int64) error }); ok {
			if err := truncater.Truncate(0); err != nil {
				return err
			}
			_, err := seeker.Seek(0, io.SeekStart)
			return err
		}
	}
	return fmt.Errorf("destination does not support reset")
}
