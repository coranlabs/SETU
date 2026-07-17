// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package dpeer

import (
	"net"
	"testing"

	"github.com/coranlabs/SETU/wire/diameter"
)

// The server must complete CER/CEA on Dial, answer DWR with DWA, and dispatch an
// application request to the handler — all over a real TCP connection.
func TestServerHandshakeAndDispatch(t *testing.T) {
	srv := &Server{
		ID: Identity{
			OriginHost: "peer.example.org", OriginRealm: "example.org",
			HostIP: net.ParseIP("127.0.0.1"), ProductName: "test-peer",
			VendorID: diameter.Vendor3GPP, AppIDs: []uint32{diameter.AppRx},
		},
		Handler: func(_ *Conn, req diameter.Message) (diameter.Message, error) {
			ans := req.Answer()
			ans.AVPs = []diameter.AVP{
				diameter.Str(diameter.AVPSessionID, 0, true, sessionOf(req)),
				diameter.ResultCode(diameter.Success),
				diameter.OriginHost("peer.example.org"),
				diameter.OriginRealm("example.org"),
			}
			return ans, nil
		},
	}
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()

	// Dial performs CER/CEA; failure returns an error.
	cli, err := Dial(srv.Addr(), Identity{
		OriginHost: "client.example.org", OriginRealm: "example.org",
		HostIP: net.ParseIP("127.0.0.1"), ProductName: "test-client", VendorID: diameter.Vendor3GPP,
	}, nil)
	if err != nil {
		t.Fatalf("CER/CEA: %v", err)
	}
	defer cli.Close()

	// DWR -> DWA(Success)
	dwa, err := cli.Send(diameter.Message{Flags: diameter.CmdRequest, Code: diameter.CmdDW, AppID: diameter.AppCommon})
	if err != nil {
		t.Fatal(err)
	}
	if got := findRC(t, dwa); got != diameter.Success {
		t.Fatalf("DWA Result-Code = %d, want Success", got)
	}

	// application request -> handler
	req := diameter.Message{
		Flags: diameter.CmdRequest, Code: diameter.CmdAA, AppID: diameter.AppRx,
		AVPs: []diameter.AVP{diameter.Str(diameter.AVPSessionID, 0, true, "s;1;1")},
	}
	ans, err := cli.Send(req)
	if err != nil {
		t.Fatal(err)
	}
	if ans.IsRequest() {
		t.Fatal("answer still marked request")
	}
	if findRC(t, ans) != diameter.Success {
		t.Fatal("app answer not Success")
	}
	if sid, _ := diameter.Find(ans.AVPs, diameter.AVPSessionID, 0); sid.StrVal() != "s;1;1" {
		t.Fatalf("Session-Id not echoed: %q", sid.StrVal())
	}
}

// Dialing a server whose handshake never completes (wrong port) must fail cleanly.
func TestDialFailure(t *testing.T) {
	if _, err := Dial("127.0.0.1:1", Identity{OriginHost: "x", OriginRealm: "y", HostIP: net.ParseIP("127.0.0.1")}, nil); err == nil {
		t.Fatal("expected dial failure")
	}
}

func sessionOf(m diameter.Message) string {
	a, _ := diameter.Find(m.AVPs, diameter.AVPSessionID, 0)
	return a.StrVal()
}

func findRC(t *testing.T, m diameter.Message) uint32 {
	t.Helper()
	a, err := diameter.Find(m.AVPs, diameter.AVPResultCode, 0)
	if err != nil {
		t.Fatalf("no Result-Code")
	}
	v, _ := a.U32Val()
	return v
}
