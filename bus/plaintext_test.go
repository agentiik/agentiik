package bus

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// An address is taken over TLS anywhere, and in plaintext to this machine alone.
func TestABusAddressIsRefusedWhereItWouldCrossANetworkInPlaintext(t *testing.T) {
	for _, taken := range []string{
		"tls://nats:4222",
		"wss://nats.example.com",
		"tls://nats-1:4222,tls://nats-2:4222",
		"nats://127.0.0.1:4222",
		"nats://localhost:4222",
		"ws://[::1]:8080",
		"tls://nats-1:4222, nats://127.0.0.1:4222",
	} {
		if err := CheckURL(taken); err != nil {
			t.Errorf("%s was refused: %v", taken, err)
		}
	}
	for _, refused := range []string{
		"nats://nats:4222",
		"ws://nats.example.com:8080",
		"tls://nats-1:4222,nats://nats-2:4222",
		"nats://10.0.0.7:4222",
		"http://nats:4222",
		"nats://",
		"nats:4222",
	} {
		if err := CheckURL(refused); !errors.Is(err, ErrPlaintext) {
			t.Errorf("%s was answered %v, and it would reach the bus in plaintext across a network", refused, err)
		}
	}
}

// A connection in plaintext to a host across the network is refused before anything is dialled,
// for the control plane and a runner alike, and the refusal does not repeat the address.
func TestABusIsNeverDialledInPlaintextAcrossANetwork(t *testing.T) {
	address := "nats://agk:s3cr3t@bus.example.com:4222"
	_, err := Open(t.Context(), Options{URL: address})
	if !errors.Is(err, ErrPlaintext) {
		t.Fatalf("the control plane's connection was answered %v", err)
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("the refusal repeats the address: %v", err)
	}
	if _, err := OpenRunner(Options{URL: address}); !errors.Is(err, ErrPlaintext) {
		t.Fatalf("a runner's connection was answered %v", err)
	}
}

// A bus that speaks nothing newer than TLS 1.1 is refused at the handshake. nats.go refuses one on
// its own too, so this holds the behaviour rather than proving the floor, which
// TestABusConnectionHoldsTheTLSFloor does.
func TestABusOfTLS11IsRefused(t *testing.T) {
	ln := natsServer(t, `{"server_id":"old","version":"2.10.0","proto":1,"max_payload":1048576,"tls_required":true}`,
		&tls.Config{Certificates: []tls.Certificate{certificate(t)}, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11})
	_, err := OpenRunner(Options{URL: "tls://" + ln})
	if err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("a TLS 1.1 bus was answered %v, and it is under the floor", err)
	}
}

// A server another one gossips is not taken where the bus is reached in plaintext, even on this
// machine, since it could be anywhere and would be reached in plaintext too.
func TestAPlaintextBusTakesNoServerItIsTold(t *testing.T) {
	ln := natsServer(t, `{"server_id":"here","version":"2.10.0","proto":1,"max_payload":1048576,"connect_urls":["10.9.8.7:4222"]}`, nil)
	b, err := OpenRunner(Options{URL: "nats://" + ln})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if found := b.conn.DiscoveredServers(); len(found) != 0 {
		t.Fatalf("a plaintext connection took %v from the server's gossip", found)
	}
}

// natsServer is enough of a NATS server on a loopback address to be connected to: it sends info,
// upgrades to TLS where given a configuration, and answers every PING. It answers the address.
func natsServer(t *testing.T, info string, config *tls.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := conn.Write([]byte("INFO " + info + "\r\n")); err != nil {
					return
				}
				if config != nil {
					server := tls.Server(conn, config)
					if server.Handshake() != nil {
						return
					}
					conn = server
				}
				lines := bufio.NewReader(conn)
				for {
					line, err := lines.ReadString('\n')
					if err != nil {
						return
					}
					if strings.HasPrefix(line, "PING") {
						conn.Write([]byte("PONG\r\n"))
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// certificate is a self-signed certificate for 127.0.0.1, which nothing needs to trust: the
// handshake it is used in fails on the version before any certificate is judged.
func certificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// A connection to the bus is made over TLS 1.2 at least, whether its address asks for TLS or its
// server insists on it.
func TestABusConnectionHoldsTheTLSFloor(t *testing.T) {
	ln := natsServer(t, `{"server_id":"here","version":"2.10.0","proto":1,"max_payload":1048576}`, nil)
	b, err := OpenRunner(Options{URL: "nats://" + ln})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if c := b.conn.Opts.TLSConfig; c == nil || c.MinVersion != tls.VersionTLS12 {
		t.Fatal("the connection's TLS configuration does not hold the floor")
	}
}

// A websocket on this machine is spoken to in plaintext, as its address says, rather than in a
// TLS its server does not speak.
func TestAPlaintextWebsocketOnThisMachineIsSpokenToInPlaintext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	first := make(chan byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		b := make([]byte, 1)
		if _, err := conn.Read(b); err == nil {
			first <- b[0]
		}
	}()
	go func() {
		if b, err := OpenRunner(Options{URL: "ws://" + ln.Addr().String()}); err == nil {
			b.Close()
		}
	}()
	select {
	case b := <-first:
		if b != 'G' {
			t.Fatalf("a ws:// connection began with 0x%02x, where a plaintext upgrade begins with GET", b)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("nothing reached the websocket")
	}
}
