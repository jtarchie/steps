package config

import (
	"strings"
	"testing"
)

// TestS3EndpointIsAHostOnly: the bucket is addressed from the endpoint's root, so a path, query or login in it would be dropped without a word; refused instead.
func TestS3EndpointIsAHostOnly(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{"https://s3.example.com", "https://s3.example.com/", "http://minio.internal:9000"} {
		err := checkS3Endpoint(endpoint)
		if err != nil {
			t.Errorf("checkS3Endpoint(%q) = %v, want it accepted", endpoint, err)
		}
	}

	for _, endpoint := range []string{"https://gw.example.com/s3", "https://s3.example.com/?x=1", "https://u:p@s3.example.com"} {
		err := checkS3Endpoint(endpoint)
		if err == nil || !strings.Contains(err.Error(), "scheme and a host only") {
			t.Errorf("checkS3Endpoint(%q) = %v, want it refused", endpoint, err)
		}
	}
}

// TestS3PutPathStaysInsideTheInputs: a path that climbs out is refused, and a name that merely starts with two dots is not one.
func TestS3PutPathStaysInsideTheInputs(t *testing.T) {
	t.Parallel()

	for _, file := range []string{"../x", "..", "out/../../x", "/etc/passwd"} {
		_, err := ParseS3PutParams(map[string]any{"file": file})
		if err == nil || !strings.Contains(err.Error(), "inside the put's inputs") {
			t.Errorf("file %q = %v, want it refused", file, err)
		}
	}

	for _, file := range []string{"..data/app.tgz", "out/..hidden"} {
		_, err := ParseS3PutParams(map[string]any{"file": file})
		if err != nil {
			t.Errorf("file %q = %v, want it accepted", file, err)
		}
	}
}
