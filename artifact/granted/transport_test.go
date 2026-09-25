package granted

import (
	"crypto/tls"
	"net/http"
	"testing"

	"github.com/agentiik/agentiik/artifact"
)

// Every object is read and posted over TLS 1.2 at least where the runner gives no client.
func TestTheObjectsOfATaskHoldTheTLSFloor(t *testing.T) {
	o, err := New(Options{Uploads: artifact.Policy{URL: "https://agentiik.example.com/objects/finance", KeyPrefix: artifact.Prefix("finance")}})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := o.client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Error("the objects' client does not hold the floor")
	}
}
