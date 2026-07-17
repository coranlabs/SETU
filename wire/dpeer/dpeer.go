// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package dpeer is a minimal but real bidirectional Diameter peer transport
// (RFC 6733): TCP/TLS framing, the CER/CEA and DWR/DWA base handshakes with peer
// allow-listing and Origin-State-Id, dispatch of inbound application requests, and
// server-initiated requests over an established association (Rx RAR/ASR). It also
// notifies on disconnect so callers can GC per-peer state.
package dpeer

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coranlabs/SETU/wire/diameter"
)

// Identity is this peer's Diameter identity, advertised in CER/CEA.
type Identity struct {
	OriginHost  string
	OriginRealm string
	HostIP      net.IP
	ProductName string
	VendorID    uint32
	AppIDs      []uint32
}

// Handler processes an inbound application request and returns the answer.
type Handler func(peer *Conn, req diameter.Message) (diameter.Message, error)

// ReadMessage reads one framed Diameter message from r.
func ReadMessage(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 20)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	total := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if total < 20 || total > 1<<24 {
		return nil, fmt.Errorf("dpeer: implausible message length %d", total)
	}
	buf := make([]byte, total)
	copy(buf, hdr)
	if _, err := io.ReadFull(r, buf[20:]); err != nil {
		return nil, err
	}
	return buf, nil
}

// Conn is a bidirectional Diameter association.
type Conn struct {
	nc       net.Conn
	id       Identity
	handler  Handler
	allowed  map[string]bool // permitted peer Origin-Realms (empty => allow any)
	stateID  uint32
	onClose  func(*Conn)
	PeerHost string // learned from the peer's CER Origin-Host (for logging)

	writeMu sync.Mutex
	pmu     sync.Mutex
	pending map[uint32]chan diameter.Message
	hbh     uint32
	closed  int32
}

func newConn(nc net.Conn, id Identity, h Handler, allowed map[string]bool, stateID uint32, onClose func(*Conn), hbhSeed uint32) *Conn {
	return &Conn{nc: nc, id: id, handler: h, allowed: allowed, stateID: stateID, onClose: onClose, pending: map[uint32]chan diameter.Message{}, hbh: hbhSeed}
}

func (c *Conn) nextHBH() uint32 { return atomic.AddUint32(&c.hbh, 1) }

func (c *Conn) write(m diameter.Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.nc.Write(m.Encode())
	return err
}

// SendRequest issues a request over the association and waits for its answer.
func (c *Conn) SendRequest(req diameter.Message) (diameter.Message, error) {
	if atomic.LoadInt32(&c.closed) != 0 {
		return diameter.Message{}, fmt.Errorf("dpeer: connection closed")
	}
	if req.HopByHop == 0 {
		req.HopByHop = c.nextHBH()
	}
	if req.EndToEnd == 0 {
		req.EndToEnd = c.nextHBH()
	}
	ch := make(chan diameter.Message, 1)
	c.pmu.Lock()
	c.pending[req.HopByHop] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, req.HopByHop)
		c.pmu.Unlock()
	}()
	if err := c.write(req); err != nil {
		return diameter.Message{}, err
	}
	select {
	case ans := <-ch:
		return ans, nil
	case <-time.After(10 * time.Second):
		return diameter.Message{}, fmt.Errorf("dpeer: request %d timed out", req.HopByHop)
	}
}

// Close terminates the association.
func (c *Conn) Close() error { return c.nc.Close() }

func (c *Conn) readLoop() {
	defer func() {
		atomic.StoreInt32(&c.closed, 1)
		c.nc.Close()
		if c.onClose != nil {
			c.onClose(c)
		}
	}()
	for {
		_ = c.nc.SetReadDeadline(time.Now().Add(300 * time.Second))
		raw, err := ReadMessage(c.nc)
		if err != nil {
			return
		}
		msg, err := diameter.DecodeMessage(raw)
		if err != nil {
			return
		}
		if msg.IsRequest() {
			if stop := c.handleRequest(msg); stop {
				return
			}
			continue
		}
		c.pmu.Lock()
		ch := c.pending[msg.HopByHop]
		c.pmu.Unlock()
		if ch != nil {
			ch <- msg
		}
	}
}

func (c *Conn) handleRequest(req diameter.Message) (stop bool) {
	var ans diameter.Message
	switch req.Code {
	case diameter.CmdCE:
		var ok bool
		ans, ok = c.answerCER(req)
		if !ok {
			_ = c.write(ans)
			return true // reject unknown peer -> drop the association
		}
	case diameter.CmdDW:
		ans = c.baseAnswer(req, diameter.Success)
	case diameter.CmdDP:
		_ = c.write(c.baseAnswer(req, diameter.Success))
		return true
	default:
		if c.handler == nil {
			ans = c.baseError(req, diameter.UnableToComply)
		} else if a, err := c.handler(c, req); err != nil {
			ans = c.baseError(req, diameter.UnableToComply)
		} else {
			ans = a
		}
	}
	_ = c.write(ans)
	return false
}

// answerCER validates the peer's realm against the allow-list (if configured) and
// records its Origin-Host. Returns (answer, accepted).
func (c *Conn) answerCER(req diameter.Message) (diameter.Message, bool) {
	if oh, err := diameter.Find(req.AVPs, diameter.AVPOriginHost, 0); err == nil {
		c.PeerHost = oh.StrVal()
	}
	if len(c.allowed) > 0 {
		realm, _ := diameter.Find(req.AVPs, diameter.AVPOriginRealm, 0)
		if !c.allowed[strings.ToLower(realm.StrVal())] {
			ans := req.Answer()
			ans.Flags |= diameter.CmdError
			ans.AVPs = []diameter.AVP{
				diameter.ResultCode(diameter.UnknownPeer),
				diameter.OriginHost(c.id.OriginHost),
				diameter.OriginRealm(c.id.OriginRealm),
			}
			return ans, false
		}
	}
	ans := req.Answer()
	avps := []diameter.AVP{
		diameter.ResultCode(diameter.Success),
		diameter.OriginHost(c.id.OriginHost),
		diameter.OriginRealm(c.id.OriginRealm),
	}
	if c.id.HostIP != nil {
		avps = append(avps, diameter.HostIPAddress(c.id.HostIP))
	}
	avps = append(avps,
		diameter.U32(diameter.AVPVendorID, 0, true, c.id.VendorID),
		diameter.Str(diameter.AVPProductName, 0, false, c.id.ProductName),
		diameter.OriginStateID(c.stateID),
	)
	for _, app := range c.id.AppIDs {
		avps = append(avps, diameter.VendorSpecificApplicationID(app),
			diameter.U32(diameter.AVPSupportedVendorID, 0, true, diameter.Vendor3GPP))
	}
	ans.AVPs = avps
	return ans, true
}

func (c *Conn) baseAnswer(req diameter.Message, rc uint32) diameter.Message {
	ans := req.Answer()
	ans.AVPs = []diameter.AVP{
		diameter.ResultCode(rc),
		diameter.OriginHost(c.id.OriginHost),
		diameter.OriginRealm(c.id.OriginRealm),
		diameter.OriginStateID(c.stateID),
	}
	return ans
}

func (c *Conn) baseError(req diameter.Message, rc uint32) diameter.Message {
	ans := c.baseAnswer(req, rc)
	ans.Flags |= diameter.CmdError
	return ans
}

// ---- Server ----

// Server accepts Diameter associations and dispatches application requests.
type Server struct {
	ID      Identity
	Handler Handler

	// AllowedRealms, if non-empty, restricts which peer Origin-Realms may connect
	// (CER from any other realm is rejected with DIAMETER_UNKNOWN_PEER).
	AllowedRealms []string
	// TLS, if set, makes Listen serve Diameter over TLS.
	TLS *tls.Config
	// OnDisconnect is called (once) when a peer association closes, so callers can
	// GC per-peer state.
	OnDisconnect func(*Conn)

	ln      net.Listener
	allowed map[string]bool
	stateID uint32
	mu      sync.Mutex
	closed  bool
}

// Listen binds the server (TLS if s.TLS is set).
func (s *Server) Listen(addr string) error {
	var (
		ln  net.Listener
		err error
	)
	if s.TLS != nil {
		ln, err = tls.Listen("tcp", addr, s.TLS)
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return err
	}
	s.ln = ln
	s.allowed = map[string]bool{}
	for _, r := range s.AllowedRealms {
		s.allowed[strings.ToLower(r)] = true
	}
	s.stateID = uint32(time.Now().Unix())
	return nil
}

// Addr returns the bound address.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Serve accepts until closed.
func (s *Server) Serve() error {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		c := newConn(nc, s.ID, s.Handler, s.allowed, s.stateID, s.OnDisconnect, 0)
		go c.readLoop()
	}
}

// Close stops the server.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

// ---- Client ----

// Client is a Diameter client association that completes CER/CEA on Dial.
type Client struct{ c *Conn }

// Dial connects to addr, completes CER/CEA, and starts the read loop. onRequest may
// be nil; when set it handles inbound server-initiated requests (e.g. Rx RAR/ASR).
// If tlsCfg is non-nil the connection is TLS.
func Dial(addr string, id Identity, onRequest Handler, tlsCfg ...*tls.Config) (*Client, error) {
	var (
		nc  net.Conn
		err error
	)
	if len(tlsCfg) > 0 && tlsCfg[0] != nil {
		nc, err = tls.Dial("tcp", addr, tlsCfg[0])
	} else {
		nc, err = net.DialTimeout("tcp", addr, 5*time.Second)
	}
	if err != nil {
		return nil, err
	}
	c := newConn(nc, id, onRequest, nil, uint32(time.Now().Unix()), nil, 1000)
	cer := diameter.Message{
		Flags: diameter.CmdRequest, Code: diameter.CmdCE, AppID: diameter.AppCommon,
		HopByHop: c.nextHBH(), EndToEnd: c.nextHBH(),
		AVPs: []diameter.AVP{
			diameter.OriginHost(id.OriginHost),
			diameter.OriginRealm(id.OriginRealm),
			diameter.HostIPAddress(id.HostIP),
			diameter.U32(diameter.AVPVendorID, 0, true, id.VendorID),
			diameter.Str(diameter.AVPProductName, 0, false, id.ProductName),
			diameter.OriginStateID(c.stateID),
		},
	}
	if err := c.write(cer); err != nil {
		nc.Close()
		return nil, err
	}
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	raw, err := ReadMessage(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("dpeer: CER failed: %w", err)
	}
	cea, err := diameter.DecodeMessage(raw)
	if err != nil {
		nc.Close()
		return nil, err
	}
	if rc, e := diameter.Find(cea.AVPs, diameter.AVPResultCode, 0); e == nil {
		if v, _ := rc.U32Val(); v != diameter.Success {
			nc.Close()
			return nil, fmt.Errorf("dpeer: CEA rejected (Result-Code %d)", v)
		}
	}
	go c.readLoop()
	return &Client{c: c}, nil
}

// Send issues a request and waits for its answer.
func (cl *Client) Send(req diameter.Message) (diameter.Message, error) { return cl.c.SendRequest(req) }

// Close disconnects.
func (cl *Client) Close() error { return cl.c.Close() }
