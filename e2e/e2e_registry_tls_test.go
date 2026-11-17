package e2e

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// tlsRegistry is an in-process registry behind a self-signed certificate, returning its host and that certificate as PEM.
func tlsRegistry(t *testing.T) (string, string) {
	t.Helper()

	server := httptest.NewTLSServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)

	cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))

	return strings.TrimPrefix(server.URL, "https://"), cert
}

func tlsPipeline(repository, extra string) string {
	return `
resources:
- name: app
  type: registry-image
  source:
    repository: ` + repository + `
` + extra + `
jobs:
- name: build
  plan:
  - task: build
    outputs: [image]
    run: echo built
  - put: app
    inputs: [image]
    params: {image: image}
`
}

// TestEndToEndRegistryImageTrustsOnlyWhatItIsTold: a registry behind a certificate no system trusts is refused, then reached with that certificate in ca_certs:, or with insecure:.
func TestEndToEndRegistryImageTrustsOnlyWhatItIsTold(t *testing.T) {
	t.Parallel()

	host, cert := tlsRegistry(t)
	layout, want := ociLayout(t, false)
	repository := host + "/team/app"

	pipeline := func(extra string) string {
		return strings.Replace(tlsPipeline(repository, extra), "run: echo built", "run: cp -R '"+layout+"/.' image/", 1)
	}

	err := stepsRun(t, []string{"run", writePipeline(t, t.TempDir(), pipeline("")), "--job", "build"})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("run with no trust: err = %v, want the certificate refused", err)
	}

	indented := "    ca_certs:\n    - |\n" + indentLines(cert, "      ")
	mustRun(t, "run", writePipeline(t, t.TempDir(), pipeline(indented)), "--job", "build")

	mustRun(t, "run", writePipeline(t, t.TempDir(), pipeline("    insecure: true\n")), "--job", "build")

	if got := tlsDigest(t, host, cert, repository); got != want {
		t.Errorf("latest = %s, want the pushed %s", got, want)
	}
}

func indentLines(text, prefix string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}

	return strings.Join(lines, "\n") + "\n"
}

// tlsDigest is the digest the TLS registry serves for repository:latest, trusting only cert.
func tlsDigest(t *testing.T, host, cert, repository string) string {
	t.Helper()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cert)) {
		t.Fatal("no certificate")
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)

	ref, err := name.ParseReference(repository + ":latest")
	if err != nil {
		t.Fatal(err)
	}

	descriptor, err := remote.Head(ref, remote.WithTransport(transport))
	if err != nil {
		t.Fatalf("HEAD %s on %s: %v", ref, host, err)
	}

	return descriptor.Digest.String()
}
