package telemetry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel"
)

func writeCA(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "t"}, IsCA: true, BasicConstraintsValid: true}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "ca.pem")

	err = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// A metrics exporter that cannot be built (plain-http endpoint with a CA configured) must not leave the already-installed trace provider and its batcher goroutine behind; goleak's TestMain catches the goroutine.
func TestStartUnwindsTracesWhenMetricsFail(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_METRICS_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE", writeCA(t))

	before := otel.GetTracerProvider()

	_, err := Start(context.Background(), "test")
	if err == nil {
		t.Fatal("Start succeeded with an http endpoint and a CA")
	}

	if otel.GetTracerProvider() != before {
		t.Error("global tracer provider left installed after a failed Start")
	}
}
