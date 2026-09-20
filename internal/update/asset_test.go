package update

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestAssetNamesFor(t *testing.T) {
	tests := []struct {
		goos   string
		goarch string
		want   []string
	}{
		{"linux", "amd64", []string{"vocat-linux-amd64"}},
		{"linux", "386", []string{"vocat-linux-386"}},
		{"linux", "arm64", []string{"vocat-linux-arm64", "vocat-linux-aarch64"}},
		{"linux", "arm", []string{"vocat-linux-armv7", "vocat-linux-arm"}},
	}
	for _, item := range tests {
		if got := assetNamesFor(item.goos, item.goarch); !reflect.DeepEqual(got, item.want) {
			t.Errorf("assetNamesFor(%q, %q) = %#v, want %#v", item.goos, item.goarch, got, item.want)
		}
	}
}

func TestDownloadAssetWithProgressVerifiesPublishedSize(t *testing.T) {
	payload := bytes.Repeat([]byte("vocat"), 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var destination bytes.Buffer
	asset := &Asset{Name: "vocat-test", BrowserDownloadURL: server.URL, Size: int64(len(payload))}
	if err := downloadAssetWithProgress(context.Background(), logger, asset, "", &destination); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(destination.Bytes(), payload) {
		t.Fatal("downloaded asset content differs")
	}

	asset.Size++
	if err := downloadAssetWithProgress(context.Background(), logger, asset, "", io.Discard); err == nil {
		t.Fatal("download with a mismatched published size succeeded")
	}
}

func TestDownloadAssetFallsBackToAcceleratedURL(t *testing.T) {
	originalClient := githubHTTPClient
	t.Cleanup(func() { githubHTTPClient = originalClient })

	directURL := "https://github.com/Remix123/VoCatFree/releases/download/v0.1.1/vocat-linux-amd64"
	payload := []byte("accelerated vocat asset")
	githubHTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() == directURL {
			return nil, &timeoutError{}
		}
		if request.URL.String() != acceleratedURL(directURL) {
			t.Fatalf("unexpected request URL: %s", request.URL)
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(bytes.NewReader(payload)),
			ContentLength: int64(len(payload)),
			Header:        make(http.Header),
			Request:       request,
		}, nil
	})}

	var destination bytes.Buffer
	asset := &Asset{Name: "vocat-test", BrowserDownloadURL: directURL, Size: int64(len(payload))}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := downloadAssetWithProgress(context.Background(), logger, asset, "", &destination); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(destination.Bytes(), payload) {
		t.Fatalf("downloaded payload = %q, want %q", destination.Bytes(), payload)
	}
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "request timed out" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

var _ error = (*timeoutError)(nil)
