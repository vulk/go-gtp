// Copyright 2019-2020 go-gtp authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package v1

import (
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	"github.com/wmnsk/go-gtp/v1/ies"
	"github.com/wmnsk/go-gtp/v1/messages"
)

// Role is a role for Kernel GTP-U.
type Role int

// Role definitions.
const (
	RoleGGSN Role = iota
	RoleSGSN
)

type tpduSet struct {
	raddr   net.Addr
	teid    uint32
	seq     uint16
	payload []byte
}

// UPlaneConn represents a U-Plane Connection of GTPv1.
type UPlaneConn struct {
	mu      sync.Mutex
	pktConn net.PacketConn
	*msgHandlerMap

	tpduCh  chan *tpduSet
	closeCh chan struct{}
	errCh   chan error

	relayMap map[uint32]*peer

	// for Linux kernel GTP with netlink
	kernGTPEnabled bool
	GTPLink        *netlink.GTP

	// RestartCounter is the RestartCounter value in Recovery IE, which represents how many
	// times the GTPv1-U endpoint is restarted.
	RestartCounter uint8
}

// DialUPlane sends Echo Request to raddr to check if the endpoint is alive and
// keep connection information.
func DialUPlane(laddr, raddr net.Addr, counter uint8, errCh chan error) (*UPlaneConn, error) {
	u := &UPlaneConn{
		mu:            sync.Mutex{},
		msgHandlerMap: defaultHandlerMap,

		tpduCh:  make(chan *tpduSet),
		closeCh: make(chan struct{}),
		errCh:   errCh,

		RestartCounter: counter,
	}

	// setup UDPConn first.
	var err error
	u.pktConn, err = net.ListenPacket(laddr.Network(), laddr.String())
	if err != nil {
		return nil, err
	}

	// if no response coming within 5 seconds, returns error.
	if err := u.pktConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}

	buf := make([]byte, 1600)
	for {
		// send EchoRequest to raddr.
		if err := u.EchoRequest(raddr); err != nil {
			return nil, err
		}

		n, _, err := u.pktConn.ReadFrom(buf)
		if err != nil {
			return nil, err
		}
		if err := u.pktConn.SetReadDeadline(time.Time{}); err != nil {
			return nil, err
		}

		// decode incoming message and let it be handled by default handler funcs.
		msg, err := messages.Parse(buf[:n])
		if err != nil {
			return nil, err
		}
		if _, ok := msg.(*messages.EchoResponse); !ok {
			continue
		}

		break
	}

	go u.serve()
	return u, nil
}

// ListenAndServeUPlane creates a new GTPv2-C *Conn and start serving.
func ListenAndServeUPlane(laddr net.Addr, counter uint8, errCh chan error) (*UPlaneConn, error) {
	u := &UPlaneConn{
		mu:            sync.Mutex{},
		msgHandlerMap: defaultHandlerMap,

		tpduCh:  make(chan *tpduSet),
		closeCh: make(chan struct{}),
		errCh:   errCh,

		RestartCounter: counter,
	}

	var err error
	u.pktConn, err = net.ListenPacket(laddr.Network(), laddr.String())
	if err != nil {
		return nil, err
	}

	go u.serve()
	return u, nil
}

// DialUPlaneKernel works similar to DialUPlane but uses Linux Kernel GTP-U
// instead of handling G-DPU message in userland.
func DialUPlaneKernel(devname string, role Role, laddr, raddr net.Addr, counter uint8, errCh chan error) (*UPlaneConn, error) {
	u := &UPlaneConn{
		mu:            sync.Mutex{},
		msgHandlerMap: defaultHandlerMap,

		tpduCh:  make(chan *tpduSet),
		closeCh: make(chan struct{}),
		errCh:   errCh,

		RestartCounter: counter,
	}

	// setup UDPConn first.
	var err error
	u.pktConn, err = net.ListenPacket(laddr.Network(), laddr.String())
	if err != nil {
		return nil, err
	}

	// if no response coming within 5 seconds, returns error.
	if err := u.pktConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}

	buf := make([]byte, 1600)
	for {
		// send EchoRequest to raddr.
		if err := u.EchoRequest(raddr); err != nil {
			return nil, err
		}

		n, _, err := u.pktConn.ReadFrom(buf)
		if err != nil {
			return nil, err
		}
		if err := u.pktConn.SetReadDeadline(time.Time{}); err != nil {
			return nil, err
		}

		// decode incoming message and let it be handled by default handler funcs.
		msg, err := messages.Parse(buf[:n])
		if err != nil {
			return nil, err
		}
		if _, ok := msg.(*messages.EchoResponse); !ok {
			continue
		}

		break
	}

	f, _ := u.pktConn.(*net.UDPConn).File()
	u.GTPLink = &netlink.GTP{
		LinkAttrs: netlink.LinkAttrs{
			Name: devname,
		},
		FD1:  int(f.Fd()),
		Role: int(role),
	}
	if err := netlink.LinkAdd(u.GTPLink); err != nil {
		return nil, errors.Wrapf(err, "failed to add device: %s", u.GTPLink.Name)
	}
	if err := netlink.LinkSetUp(u.GTPLink); err != nil {
		return nil, errors.Wrapf(err, "failed to setup device: %s", u.GTPLink.Name)
	}
	if err := netlink.LinkSetMTU(u.GTPLink, 1500); err != nil {
		return nil, errors.Wrapf(err, "failed to set MTU for device: %s", u.GTPLink.Name)
	}
	u.kernGTPEnabled = true

	go u.serve()
	return u, nil
}

// ListenAndServeUPlaneKernel works similar to ListenAndServeUPlane but uses Linux Kernel GTP-U
// instead of handling G-DPU message in userland.
func ListenAndServeUPlaneKernel(devname string, role Role, laddr net.Addr, counter uint8, errCh chan error) (*UPlaneConn, error) {
	u := &UPlaneConn{
		mu:            sync.Mutex{},
		msgHandlerMap: defaultHandlerMap,

		tpduCh:  make(chan *tpduSet),
		closeCh: make(chan struct{}),
		errCh:   errCh,

		RestartCounter: counter,
	}

	var err error
	u.pktConn, err = net.ListenPacket(laddr.Network(), laddr.String())
	if err != nil {
		return nil, err
	}

	f, _ := u.pktConn.(*net.UDPConn).File()
	u.GTPLink = &netlink.GTP{
		LinkAttrs: netlink.LinkAttrs{
			Name: devname,
		},
		FD1:  int(f.Fd()),
		Role: int(role),
	}
	if err := netlink.LinkAdd(u.GTPLink); err != nil {
		return nil, errors.Wrapf(err, "failed to add device: %s", u.GTPLink.Name)
	}
	if err := netlink.LinkSetUp(u.GTPLink); err != nil {
		return nil, errors.Wrapf(err, "failed to setup device: %s", u.GTPLink.Name)
	}
	if err := netlink.LinkSetMTU(u.GTPLink, 1500); err != nil {
		return nil, errors.Wrapf(err, "failed to set MTU for device: %s", u.GTPLink.Name)
	}
	u.kernGTPEnabled = true

	go u.serve()
	return u, nil
}

func (u *UPlaneConn) serve() {
	buf := make([]byte, 1500)
	for {
		select {
		case <-u.closed():
			return
		default:
			// do nothing and go forward.
		}

		n, raddr, err := u.pktConn.ReadFrom(buf)
		if err != nil {
			return
		}

		// just forward T-PDU instead of passing it to reader if relayer is
		// configured and the message type is T-PDU.
		if len(u.relayMap) != 0 && buf[1] == messages.MsgTypeTPDU {
			// ignore if the packet size is smaller than minimum header size
			if n < 11 {
				continue
			}

			u.mu.Lock()
			peer, ok := u.relayMap[binary.BigEndian.Uint32(buf[4:8])]
			u.mu.Unlock()
			if !ok {
				continue
			}

			// just use original packet not to get it slow.
			binary.BigEndian.PutUint32(buf[4:8], peer.teid)
			if _, err := peer.srcConn.WriteTo(buf, peer.addr); err != nil {
				go func() {
					u.errCh <- err
				}()
			}
			continue
		}

		msg, err := messages.Parse(buf[:n])
		if err != nil {
			continue
		}

		if err := u.handleMessage(raddr, msg); err != nil {
			// errors should be handled by user
			go func() {
				u.errCh <- err
			}()
			continue
		}
	}
}

// ReadFrom reads a packet from the connection,
// copying the payload into p. It returns the number of
// bytes copied into p and the return address that
// was on the packet.
// It returns the number of bytes read (0 <= n <= len(p))
// and any error encountered. Callers should always process
// the n > 0 bytes returned before considering the error err.
// ReadFrom can be made to time out and return
// an Error with Timeout() == true after a fixed time limit;
// see SetDeadline and SetReadDeadline.
//
// Note that valid GTP-U packets handled by Kernel can NOT be retrieved by this.
func (u *UPlaneConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	return u.pktConn.ReadFrom(p)
}

// ReadFromGTP reads a packet from the connection, copying the payload without
// GTP header into p. It returns the number of bytes copied into p, the return
// address that was on the packet, TEID in the GTP header.
//
// Note that valid GTP-U packets handled by Kernel can NOT be retrieved by this.
func (u *UPlaneConn) ReadFromGTP(p []byte) (n int, addr net.Addr, teid uint32, err error) {
	select {
	case <-u.closed():
		return
	case tpdu, ok := <-u.tpduCh:
		if !ok {
			err = ErrConnNotOpened
			return
		}
		n = copy(p, tpdu.payload)
		addr = tpdu.raddr
		teid = tpdu.teid
		return
	}
}

// WriteTo writes a packet with payload p to addr.
// WriteTo can be made to time out and return
// an Error with Timeout() == true after a fixed time limit;
// see SetDeadline and SetWriteDeadline.
// On packet-oriented connections, write timeouts are rare.
func (u *UPlaneConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	return u.pktConn.WriteTo(p, addr)
}

// WriteToGTP writes a packet with TEID and payload to addr.
func (u *UPlaneConn) WriteToGTP(teid uint32, p []byte, addr net.Addr) (n int, err error) {
	b, err := Encapsulate(teid, p).Marshal()
	if err != nil {
		return
	}

	if _, err = u.pktConn.WriteTo(b, addr); err != nil {
		return
	}
	return len(b), nil
}

// closed would be used in multiple goroutines.
// never send struct{}{} to it; instead, use close(u.closeCh).
func (u *UPlaneConn) closed() <-chan struct{} {
	return u.closeCh
}

// Close closes the connection.
// Any blocked Read or Write operations will be unblocked and return errors.
func (u *UPlaneConn) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.msgHandlerMap = defaultHandlerMap
	close(u.closeCh)
	u.relayMap = nil

	if u.kernGTPEnabled {
		_ = netlink.LinkDel(u.GTPLink)
	}

	// triggers error in blocking Read() / Write() after 1ms.
	return u.pktConn.SetDeadline(time.Now().Add(1 * time.Millisecond))
}

// LocalAddr returns the local network address.
func (u *UPlaneConn) LocalAddr() net.Addr {
	return u.pktConn.LocalAddr()
}

// SetDeadline sets the read and write deadlines associated
// with the connection. It is equivalent to calling both
// SetReadDeadline and SetWriteDeadline.
//
// A deadline is an absolute time after which I/O operations
// fail with a timeout (see type Error) instead of
// blocking. The deadline applies to all future and pending
// I/O, not just the immediately following call to Read or
// Write. After a deadline has been exceeded, the connection
// can be refreshed by setting a deadline in the future.
//
// An idle timeout can be implemented by repeatedly extending
// the deadline after successful Read or Write calls.
//
// A zero value for t means I/O operations will not time out.
func (u *UPlaneConn) SetDeadline(t time.Time) error {
	return u.pktConn.SetDeadline(t)
}

// SetReadDeadline sets the deadline for future Read calls
// and any currently-blocked Read call.
// A zero value for t means Read will not time out.
func (u *UPlaneConn) SetReadDeadline(t time.Time) error {
	return u.pktConn.SetReadDeadline(t)
}

// SetWriteDeadline sets the deadline for future Write calls
// and any currently-blocked Write call.
// Even if write times out, it may return n > 0, indicating that
// some of the data was successfully written.
// A zero value for t means Write will not time out.
func (u *UPlaneConn) SetWriteDeadline(t time.Time) error {
	return u.pktConn.SetWriteDeadline(t)
}

// AddHandler adds a message handler to *UPlaneConn.
//
// By adding HandlerFuncs, *UPlaneConn (and *Session, *Bearer created by the *UPlaneConn) will handle
// the specified type of message with it's paired HandlerFunc when receiving.
// Messages without registered handlers are just ignored and discarded and the user will
// get ErrNoHandlersFound error.
//
// This should be performed just after creating *UPlaneConn, otherwise the user cannot retrieve
// any values, which is in most cases vital to continue working as a node, from the incoming
// messages.
//
// HandlerFuncs for EchoResponse and ErrorIndication are registered by default.
// These HandlerFuncs can be overwritten by specifying messages.MsgTypeEchoResponse and/or
// messages.MsgTypeErrorIndication as msgType parameter.
func (u *UPlaneConn) AddHandler(msgType uint8, fn HandlerFunc) {
	u.msgHandlerMap.store(msgType, fn)
}

// AddHandlers adds multiple handler funcs at a time.
//
// See AddHandler for detailed usage.
func (u *UPlaneConn) AddHandlers(funcs map[uint8]HandlerFunc) {
	for msgType, fn := range funcs {
		u.msgHandlerMap.store(msgType, fn)
	}
}

func (u *UPlaneConn) handleMessage(senderAddr net.Addr, msg messages.Message) error {
	handle, ok := u.msgHandlerMap.load(msg.MessageType())
	if !ok {
		return ErrNoHandlersFound
	}
	go func() {
		if err := handle(u, senderAddr, msg); err != nil {
			u.errCh <- err
		}
	}()

	return nil
}

// EchoRequest sends a EchoRequest.
func (u *UPlaneConn) EchoRequest(raddr net.Addr) error {
	b, err := messages.NewEchoRequest(0, ies.NewRecovery(u.RestartCounter)).Marshal()
	if err != nil {
		return err
	}

	if _, err := u.pktConn.WriteTo(b, raddr); err != nil {
		return err
	}
	return nil
}

// EchoResponse sends a EchoResponse.
func (u *UPlaneConn) EchoResponse(raddr net.Addr) error {
	b, err := messages.NewEchoResponse(0, ies.NewRecovery(u.RestartCounter)).Marshal()
	if err != nil {
		return err
	}

	if _, err := u.pktConn.WriteTo(b, raddr); err != nil {
		return err
	}
	return nil
}

// ErrorIndication just sends ErrorIndication message.
func (u *UPlaneConn) ErrorIndication(raddr net.Addr, received messages.Message) error {
	addr := strings.Split(raddr.String(), ":")[0]
	errInd, err := messages.NewErrorIndication(
		0, received.Sequence(),
		ies.NewTEIDDataI(received.TEID()),
		ies.NewGSNAddress(addr),
	).Marshal()
	if err != nil {
		return err
	}

	if _, err := u.WriteTo(errInd, raddr); err != nil {
		return err
	}
	return nil
}

// RespondTo sends a message(specified with "toBeSent" param) in response to
// a message(specified with "received" param).
//
// This is to make it easier to handle SequenceNumber.
func (u *UPlaneConn) RespondTo(raddr net.Addr, received, toBeSent messages.Message) error {
	toBeSent.SetSequenceNumber(received.Sequence())
	b := make([]byte, toBeSent.MarshalLen())
	if err := toBeSent.MarshalTo(b); err != nil {
		return err
	}

	if _, err := u.WriteTo(b, raddr); err != nil {
		return err
	}
	return nil
}

// Restarts returns the number of restarts in uint8.
func (u *UPlaneConn) Restarts() uint8 {
	return u.RestartCounter
}
