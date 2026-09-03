package gateway

import (
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"testing"
)

// TestComponentsDefaultToTheStandardLogger pins the invariant the gateway's
// --log-file rests on: a component given no Logger of its own logs through
// [log.Default], so one [log.SetOutput] moves everything the process writes into
// the file. A component that built itself a logger on os.Stderr instead would
// keep writing to the console, and nothing would say so.
func TestComponentsDefaultToTheStandardLogger(t *testing.T) {
	// Constructing these logs a line or two, which is the very thing being
	// asserted: redirecting the standard logger takes it out of the test output.
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	peerURL, err := url.Parse("https://gateway.test:8443")
	if err != nil {
		t.Fatalf("parsing the peer URL: %v", err)
	}
	forward, err := NewForward(ForwardConfig{Peer: peerURL})
	if err != nil {
		t.Fatalf("NewForward: %v", err)
	}
	peerAuth, err := NewPeerAuth(PeerAuthOptions{TokenFiles: []string{writeTokens(t, testToken)}})
	if err != nil {
		t.Fatalf("NewPeerAuth: %v", err)
	}
	peerTLS, err := NewPeerTLS(PeerTLSOptions{})
	if err != nil {
		t.Fatalf("NewPeerTLS: %v", err)
	}
	peers, err := NewKubernetesPeers(KubernetesPeerOptions{
		Service:      "img-gateway/oci-distribution-gateway",
		Port:         8443,
		SelfName:     "gateway-0",
		APIServerURL: "https://api.test",
		Client:       &http.Client{},
	})
	if err != nil {
		t.Fatalf("NewKubernetesPeers: %v", err)
	}
	replication, err := NewCacheReplication(ReplicationConfig{
		Peers:  StaticPeers{"https://peer.test:8443"},
		SelfID: "gateway-0",
	})
	if err != nil {
		t.Fatalf("NewCacheReplication: %v", err)
	}

	for _, tc := range []struct {
		component string
		logger    *log.Logger
	}{
		{"the serving handler", New().log},
		{"the forwarder", forward.log},
		{"client authentication", peerAuth.log},
		{"TLS material", peerTLS.log},
		{"peer discovery", peers.log},
		{"cache replication", replication.log},
	} {
		if tc.logger != log.Default() {
			t.Errorf("%s logs through a logger of its own, so --log-file would not capture it", tc.component)
		}
	}
}
