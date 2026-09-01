package shim

// The metadata side of the drain watcher, against servers standing in for
// each cloud. The frame side — a draining notice crossing the wire and
// becoming an eviction — is covered by the venue's scripted-shim tests; what
// lives here is the part those cannot see: telling the clouds apart, and
// reading each one's notice.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// swapMetadataBase points the watcher at a stand-in metadata service.
func swapMetadataBase(t *testing.T, url string) {
	t.Helper()

	previous := metadataBase
	metadataBase = url

	t.Cleanup(func() { metadataBase = previous })
}

// drainClient is the client watchForDrain builds, without the session.
func drainClient() *http.Client {
	return &http.Client{Timeout: imdsTimeout, Transport: &http.Transport{Proxy: nil}}
}

// fakeGCEMetadata answers as a GCE metadata service: the Metadata-Flavor
// header on every response, an instance id, and a scripted preempted flag.
func fakeGCEMetadata(t *testing.T, preempted string) {
	t.Helper()
	fakeGCEMetadataFull(t, preempted, "NONE")
}

// fakeGCEMetadataFull also scripts the maintenance-event key.
func fakeGCEMetadataFull(t *testing.T, preempted, maintenance string) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "missing Metadata-Flavor", http.StatusForbidden)

			return
		}

		w.Header().Set("Metadata-Flavor", "Google")

		switch req.URL.Path {
		case "/computeMetadata/v1/instance/id":
			_, _ = w.Write([]byte("1234567890"))
		case "/computeMetadata/v1/instance/preempted":
			_, _ = w.Write([]byte(preempted))
		case "/computeMetadata/v1/instance/maintenance-event":
			_, _ = w.Write([]byte(maintenance))
		default:
			http.NotFound(w, req)
		}
	}))

	t.Cleanup(server.Close)
	swapMetadataBase(t, server.URL)
}

// fakeIMDS answers as an EC2 metadata service: the token handshake, and a
// scripted spot notice.
func fakeIMDS(t *testing.T, notice string) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodPut && req.URL.Path == "/latest/api/token":
			_, _ = w.Write([]byte("imds-token"))
		case req.URL.Path == "/latest/meta-data/spot/instance-action" && notice != "":
			_, _ = w.Write([]byte(notice))
		default:
			http.NotFound(w, req)
		}
	}))

	t.Cleanup(server.Close)
	swapMetadataBase(t, server.URL)
}

func TestDetectCloudSettlesOnGCE(t *testing.T) {
	fakeGCEMetadata(t, "FALSE")

	cloud, settled := detectCloud(context.Background(), drainClient())
	if !settled || cloud != cloudGCE {
		t.Fatalf("detectCloud = %v, %v — want GCE, settled", cloud, settled)
	}
}

func TestDetectCloudSettlesOnEC2(t *testing.T) {
	fakeIMDS(t, "")

	cloud, settled := detectCloud(context.Background(), drainClient())
	if !settled || cloud != cloudEC2 {
		t.Fatalf("detectCloud = %v, %v — want EC2, settled", cloud, settled)
	}
}

func TestDetectCloudGivesUpOffCloud(t *testing.T) {
	// A server that answers, but as neither cloud: no Metadata-Flavor
	// header anywhere. A 200 alone must not read as GCE — any captive
	// portal answers 200.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(server.Close)
	swapMetadataBase(t, server.URL)

	// The IMDS token PUT gets a 200 "hello" here too, which reads as a
	// granted token — so this asserts only that GCE is not claimed without
	// its header.
	cloud, _ := detectCloud(context.Background(), drainClient())
	if cloud == cloudGCE {
		t.Fatal("a server without the Metadata-Flavor header was read as GCE")
	}
}

func TestDetectCloudGivesUpWhenNothingAnswers(t *testing.T) {
	// A server that is already gone: the dial itself fails, which is what a
	// machine outside any cloud looks like.
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	swapMetadataBase(t, server.URL)

	_, answered := detectCloud(context.Background(), drainClient())
	if answered {
		t.Fatal("an unreachable metadata service read as answering")
	}
}

func TestDetectCloudDoesNotSettleEC2WithoutAToken(t *testing.T) {
	// GCE's metadata service answers the IMDS token PUT too — with a 404.
	// An answer without a token must not settle EC2: on a GCE machine whose
	// identifying probe blipped, that misread would poll EC2 spot paths for
	// the life of the session and never see a real preemption.
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	swapMetadataBase(t, server.URL)

	cloud, answered := detectCloud(context.Background(), drainClient())
	if cloud != cloudUnknown || !answered {
		t.Fatalf("detectCloud = %v, %v — want unknown but answered", cloud, answered)
	}
}

func TestSettleCloudRetriesAfterABlipAndSettlesGCE(t *testing.T) {
	// The sharp scenario: a GCE machine whose identifying probe fails once
	// (a 5xx — carrying the Metadata-Flavor header, as every GCE response
	// does), while the token PUT gets GCE's 404. The watcher must neither
	// stop nor settle on EC2; the next tick's probe settles GCE.
	var blipped atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")

		if req.URL.Path == "/computeMetadata/v1/instance/id" {
			if blipped.CompareAndSwap(false, true) {
				http.Error(w, "transient", http.StatusInternalServerError)

				return
			}

			_, _ = w.Write([]byte("1234567890"))

			return
		}

		http.NotFound(w, req)
	}))
	t.Cleanup(server.Close)
	swapMetadataBase(t, server.URL)

	cloud := cloudUnknown

	done := settleCloud(context.Background(), drainClient(), &cloud)
	if done || cloud != cloudUnknown {
		t.Fatalf("first probe: done=%v cloud=%v — want the watcher alive and unsettled", done, cloud)
	}

	done = settleCloud(context.Background(), drainClient(), &cloud)
	if done || cloud != cloudGCE {
		t.Fatalf("second probe: done=%v cloud=%v — want GCE settled", done, cloud)
	}
}

func TestSettleCloudStopsForGoodWhenNothingAnswers(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	swapMetadataBase(t, server.URL)

	cloud := cloudUnknown

	done := settleCloud(context.Background(), drainClient(), &cloud)
	if !done {
		t.Fatal("a dead metadata address kept the watcher polling")
	}
}

func TestGCENoticeReportsPreemption(t *testing.T) {
	fakeGCEMetadata(t, "TRUE")

	notice, terminal := gceNotice(context.Background(), drainClient())
	if !terminal || notice.Reason != "GCE preemption" || !notice.Terminal {
		t.Fatalf("gceNotice = %+v, %v — want a terminal GCE preemption", notice, terminal)
	}
}

func TestGCENoticeIgnoresTheOrdinaryFalse(t *testing.T) {
	// FALSE is the flag's value on every healthy instance from first boot;
	// reading it as a notice would evict every worker on its first poll.
	fakeGCEMetadata(t, "FALSE")

	notice, terminal := gceNotice(context.Background(), drainClient())
	if terminal || notice.Reason != "" {
		t.Fatalf("gceNotice on FALSE = %+v, %v — want no notice", notice, terminal)
	}
}

func TestGCENoticeIgnoresAnEmptyAnswer(t *testing.T) {
	// A 200 with nothing in it is not a notice; fabricating one would
	// terminate a healthy machine — the same rule imdsGet holds for EC2.
	fakeGCEMetadata(t, "")

	notice, terminal := gceNotice(context.Background(), drainClient())
	if terminal || notice.Reason != "" {
		t.Fatalf("gceNotice on empty = %+v, %v — want no notice", notice, terminal)
	}
}

func TestGCENoticeReportsATerminatingMaintenanceEvent(t *testing.T) {
	// What instances.simulateMaintenanceEvent actually does to a spot
	// instance — measured against the real service: preempted stays FALSE
	// and maintenance-event announces the termination.
	fakeGCEMetadataFull(t, "FALSE", "TERMINATE_ON_HOST_MAINTENANCE")

	notice, terminal := gceNotice(context.Background(), drainClient())
	if !terminal || !notice.Terminal || notice.Reason == "" {
		t.Fatalf("gceNotice = %+v, %v — want a terminal maintenance notice", notice, terminal)
	}
}

func TestGCENoticeIgnoresALiveMigration(t *testing.T) {
	// A MIGRATE event pauses the machine and it survives; reading it as an
	// eviction would destroy a healthy worker over routine host maintenance.
	fakeGCEMetadataFull(t, "FALSE", "MIGRATE_ON_HOST_MAINTENANCE")

	notice, terminal := gceNotice(context.Background(), drainClient())
	if terminal || notice.Reason != "" {
		t.Fatalf("gceNotice on MIGRATE = %+v, %v — want no notice", notice, terminal)
	}
}

func TestGCENoticeIgnoresAMissingFlag(t *testing.T) {
	// A metadata server that answers everything else but 404s the preempted
	// path — not a notice, and not an error worth ending the watch over.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		http.NotFound(w, req)
	}))
	t.Cleanup(server.Close)
	swapMetadataBase(t, server.URL)

	notice, terminal := gceNotice(context.Background(), drainClient())
	if terminal || notice.Reason != "" {
		t.Fatalf("gceNotice on 404 = %+v, %v — want no notice", notice, terminal)
	}
}

func TestCloudNoticeRoutesEachCloud(t *testing.T) {
	fakeGCEMetadata(t, "TRUE")

	notice, terminal := cloudNotice(context.Background(), drainClient(), cloudGCE)
	if !terminal || notice.Reason != "GCE preemption" {
		t.Fatalf("cloudNotice(GCE) = %+v, %v", notice, terminal)
	}

	fakeIMDS(t, `{"action":"terminate","time":"2030-01-01T00:00:00Z"}`)

	notice, terminal = cloudNotice(context.Background(), drainClient(), cloudEC2)
	if !terminal || notice.Reason != "EC2 spot terminate" || notice.Deadline != "2030-01-01T00:00:00Z" {
		t.Fatalf("cloudNotice(EC2) = %+v, %v", notice, terminal)
	}
}
