package updates

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadVerifiesBytesAndRefusesRedirect(t *testing.T) {
	data := []byte("signed artifact bytes")
	sum := sha256.Sum256(data)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/ok", 302)
			return
		}
		w.Write(data)
	}))
	defer ts.Close()
	// 测试仅信任当前测试服务器的证书，不关闭证书验证。
	roots := x509.NewCertPool()
	roots.AddCert(ts.Certificate())
	old := http.DefaultTransport
	transport := old.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = old; transport.CloseIdleConnections() }()
	release := Release{URL: ts.URL + "/ok", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	target := filepath.Join(t.TempDir(), "artifact")
	if e := Download(context.Background(), release, target); e != nil {
		t.Fatal(e)
	}
	actual, _ := os.ReadFile(target)
	if !bytes.Equal(actual, data) {
		t.Fatal("artifact bytes")
	}
	for _, change := range []func(*Release){func(r *Release) { r.URL = ts.URL + "/redirect" }, func(r *Release) { r.SHA256 = hex.EncodeToString(make([]byte, 32)) }, func(r *Release) { r.Size-- }} {
		r := release
		change(&r)
		if e := Download(context.Background(), r, filepath.Join(t.TempDir(), "artifact")); e == nil {
			t.Fatal("unsafe artifact accepted")
		}
	}
}
func TestDockerArchiveRejectsExtraTagsAndWrongImage(t *testing.T) {
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	sum := sha256.Sum256(config)
	digest := hex.EncodeToString(sum[:])
	release := Release{Image: "mfconnect-server:0.4.1-docker.1", ImageID: "sha256:" + digest}
	create := func(tags []string, configuration []byte) string {
		p := filepath.Join(t.TempDir(), "image.tar.gz")
		f, _ := os.Create(p)
		gz := gzip.NewWriter(f)
		tw := tar.NewWriter(gz)
		manifest, _ := json.Marshal([]map[string]any{{"Config": "blobs/sha256/" + digest, "RepoTags": tags, "Layers": []string{}}})
		for name, data := range map[string][]byte{"manifest.json": manifest, "blobs/sha256/" + digest: configuration} {
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))})
			tw.Write(data)
		}
		tw.Close()
		gz.Close()
		f.Close()
		return p
	}
	if e := ValidateImageArchive(create([]string{release.Image}, config), release); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{create([]string{release.Image, "unrelated:latest"}, config), create([]string{release.Image}, []byte("modified config"))} {
		if ValidateImageArchive(p, release) == nil {
			t.Fatal("unsafe image accepted")
		}
	}
}
