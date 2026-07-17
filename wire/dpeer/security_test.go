// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package dpeer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/coranlabs/SETU/wire/diameter"
)

func idFor(host, realm string) Identity {
	return Identity{OriginHost: host, OriginRealm: realm, HostIP: net.ParseIP("127.0.0.1"),
		ProductName: "test", VendorID: diameter.Vendor3GPP, AppIDs: []uint32{diameter.AppRx}}
}

// A peer whose Origin-Realm is not in AllowedRealms must be rejected at CER.
func TestPeerAllowList(t *testing.T) {
	srv := &Server{ID: idFor("gw.trusted.org", "trusted.org"), AllowedRealms: []string{"trusted.org"}}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	// allowed realm connects fine
	if cli, err := Dial(srv.Addr(), idFor("pcscf.trusted.org", "trusted.org"), nil); err != nil {
		t.Fatalf("allowed peer rejected: %v", err)
	} else {
		cli.Close()
	}
	// disallowed realm is rejected at CER (DIAMETER_UNKNOWN_PEER)
	if _, err := Dial(srv.Addr(), idFor("attacker.evil.org", "evil.org"), nil); err == nil {
		t.Fatal("disallowed peer was accepted — allow-list not enforced")
	}
}

// Diameter over TLS must complete CER/CEA and carry an application request.
func TestDiameterOverTLS(t *testing.T) {
	cert := selfSigned(t)
	srv := &Server{
		ID:  idFor("gw.example.org", "example.org"),
		TLS: &tls.Config{Certificates: []tls.Certificate{cert}},
		Handler: func(_ *Conn, req diameter.Message) (diameter.Message, error) {
			ans := req.Answer()
			ans.AVPs = []diameter.AVP{diameter.ResultCode(diameter.Success),
				diameter.OriginHost("gw.example.org"), diameter.OriginRealm("example.org")}
			return ans, nil
		},
	}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	cli, err := Dial(srv.Addr(), idFor("pcscf.example.org", "example.org"), nil,
		&tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("TLS dial/CER failed: %v", err)
	}
	defer cli.Close()

	ans, err := cli.Send(diameter.Message{Flags: diameter.CmdRequest, Code: diameter.CmdAA, AppID: diameter.AppRx,
		AVPs: []diameter.AVP{diameter.Str(diameter.AVPSessionID, 0, true, "s;1;1")}})
	if err != nil {
		t.Fatal(err)
	}
	if rc, _ := diameter.Find(ans.AVPs, diameter.AVPResultCode, 0); func() uint32 { v, _ := rc.U32Val(); return v }() != diameter.Success {
		t.Fatal("app request over TLS did not succeed")
	}
}

// OnDisconnect must fire when a peer association closes (used to GC per-peer state).
func TestOnDisconnectFires(t *testing.T) {
	gone := make(chan string, 1)
	srv := &Server{
		ID:           idFor("gw.example.org", "example.org"),
		Handler:      func(_ *Conn, req diameter.Message) (diameter.Message, error) { return req.Answer(), nil },
		OnDisconnect: func(c *Conn) { gone <- c.PeerHost },
	}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	cli, err := Dial(srv.Addr(), idFor("pcscf.example.org", "example.org"), nil)
	if err != nil {
		t.Fatal(err)
	}
	cli.Close()
	select {
	case host := <-gone:
		if host != "pcscf.example.org" {
			t.Fatalf("OnDisconnect PeerHost = %q", host)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnDisconnect did not fire on peer close")
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
