// Package socks5 implements a SOCKS5 proxy server (RFC 1928) that tunnels
// connections through the USB channel to the B-side forwarder.
package socks5

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"usbx/internal/channel"
)

const (
	SocksVersion = 0x05

	// Auth methods.
	AuthNone     = 0x00
	AuthPassword = 0x02
	AuthNoAccept = 0xFF

	// Commands.
	CmdConnect = 0x01

	// Address types.
	AddrIPv4   = 0x01
	AddrDomain = 0x03
	AddrIPv6   = 0x04

	// Reply codes.
	RepSuccess         = 0x00
	RepGeneralFailure  = 0x01
	RepNotAllowed      = 0x02
	RepNetUnreachable  = 0x03
	RepHostUnreachable = 0x04
	RepRefused         = 0x05
	RepTTLExpired      = 0x06
	RepCmdNotSupported = 0x07
	RepAddrNotSupported = 0x08
)

// Server is a SOCKS5 proxy server.
type Server struct {
	listenAddr string
	mux        *channel.Mux
	dialer     net.Dialer

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new SOCKS5 server that tunnels through the USB channel mux.
func New(listenAddr string, mux *channel.Mux) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		listenAddr: listenAddr,
		mux:        mux,
		dialer: net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start begins listening for SOCKS5 connections.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("socks5: listen: %w", err)
	}
	defer ln.Close()

	log.Printf("[socks5] listening on %s", s.listenAddr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				return nil
			default:
				log.Printf("[socks5] accept error: %v", err)
				continue
			}
		}

		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

// Shutdown stops the SOCKS5 server.
func (s *Server) Shutdown() {
	s.cancel()
	s.wg.Wait()
}

func (s *Server) handleConn(client net.Conn) {
	defer s.wg.Done()
	defer client.Close()

	// 1. Method negotiation.
	if err := s.handleHandshake(client); err != nil {
		log.Printf("[socks5] handshake error: %v", err)
		return
	}

	// 2. Request.
	target, err := s.handleRequest(client)
	if err != nil {
		log.Printf("[socks5] request error: %v", err)
		return
	}

	// 3. Open USB stream to B-side (pipeline mode: don't wait for ACK).
	// This saves one 20-50ms round-trip over high-latency USB-net channels.
	stream, err := s.mux.OpenStreamPipeline(s.ctx, target)
	if err != nil {
		log.Printf("[socks5] open stream to %s: %v", target, err)
		s.sendReply(client, RepHostUnreachable, nil)
		return
	}
	defer stream.Close()

	// 4. Send success reply immediately (optimistic).
	s.sendReply(client, RepSuccess, nil)

	// 5. Bidirectional relay with async failure detection.
	var wg sync.WaitGroup
	wg.Add(3)

	// Monitor for async stream failure.
	done := make(chan struct{})
	go func() {
		defer wg.Done()
		select {
		case ackErr := <-stream.AckCh():
			if ackErr != nil {
				log.Printf("[socks5] stream %d async error: %v", stream.StreamID(), ackErr)
				client.Close()
				stream.Close()
			}
		case <-done:
		}
	}()

	// Client → Stream (A→B).
	go func() {
		defer wg.Done()
		defer func() {
			stream.Flush()
			stream.Close()
			close(done)
		}()
		io.Copy(stream, client)
	}()

	// Stream → Client (B→A).
	go func() {
		defer wg.Done()
		defer client.Close()
		io.Copy(client, stream)
	}()

	wg.Wait()
}

func (s *Server) handleHandshake(conn net.Conn) error {
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Read version + number of methods.
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read handshake header: %w", err)
	}

	ver := header[0]
	if ver != SocksVersion {
		return fmt.Errorf("unsupported SOCKS version: %d", ver)
	}

	nmethods := int(header[1])
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(conn, methods); err != nil {
		return fmt.Errorf("read methods: %w", err)
	}

	// Accept no-auth (0x00) if offered, otherwise reject.
	authMethod := byte(AuthNoAccept)
	for _, m := range methods {
		if m == AuthNone {
			authMethod = AuthNone
			break
		}
	}

	// Send method selection.
	resp := []byte{SocksVersion, authMethod}
	if _, err := conn.Write(resp); err != nil {
		return fmt.Errorf("write method selection: %w", err)
	}

	if authMethod == AuthNoAccept {
		return fmt.Errorf("no acceptable auth method")
	}

	return nil
}

// parseAddr parses the address from a SOCKS5 request.
// Returns "host:port" string and the number of bytes consumed.
func parseAddr(data []byte) (string, int, error) {
	if len(data) < 1 {
		return "", 0, fmt.Errorf("short address")
	}

	switch data[0] {
	case AddrIPv4:
		if len(data) < 7 {
			return "", 0, fmt.Errorf("short IPv4 address")
		}
		ip := net.IP(data[1:5]).String()
		port := int(data[5])<<8 | int(data[6])
		return net.JoinHostPort(ip, fmt.Sprintf("%d", port)), 7, nil

	case AddrDomain:
		if len(data) < 2 {
			return "", 0, fmt.Errorf("short domain address")
		}
		domainLen := int(data[1])
		if len(data) < 2+domainLen+2 {
			return "", 0, fmt.Errorf("short domain address body")
		}
		domain := string(data[2 : 2+domainLen])
		off := 2 + domainLen
		port := int(data[off])<<8 | int(data[off+1])
		return net.JoinHostPort(domain, fmt.Sprintf("%d", port)), off + 2, nil

	case AddrIPv6:
		if len(data) < 19 {
			return "", 0, fmt.Errorf("short IPv6 address")
		}
		ip := net.IP(data[1:17]).String()
		port := int(data[17])<<8 | int(data[18])
		return net.JoinHostPort(ip, fmt.Sprintf("%d", port)), 19, nil

	default:
		return "", 0, fmt.Errorf("unsupported address type: %d", data[0])
	}
}

func (s *Server) handleRequest(conn net.Conn) (string, error) {
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Read version + command + reserved + address type (4 bytes).
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", fmt.Errorf("read request header: %w", err)
	}

	if header[0] != SocksVersion {
		return "", fmt.Errorf("unsupported SOCKS version: %d", header[0])
	}

	if header[1] != CmdConnect {
		s.sendReply(conn, RepCmdNotSupported, nil)
		return "", fmt.Errorf("unsupported command: %d", header[1])
	}

	addrType := header[3]

	// Read address based on type.
	var target string
	switch addrType {
	case AddrIPv4:
		body := make([]byte, 4+2) // 4 bytes IPv4 + 2 bytes port
		if _, err := io.ReadFull(conn, body); err != nil {
			return "", fmt.Errorf("read IPv4 body: %w", err)
		}
		ip := net.IP(body[0:4]).String()
		port := int(body[4])<<8 | int(body[5])
		target = net.JoinHostPort(ip, fmt.Sprintf("%d", port))

	case AddrDomain:
		lenByte := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenByte); err != nil {
			return "", fmt.Errorf("read domain length: %w", err)
		}
		domainLen := int(lenByte[0])
		body := make([]byte, domainLen+2) // domain + 2 bytes port
		if _, err := io.ReadFull(conn, body); err != nil {
			return "", fmt.Errorf("read domain body: %w", err)
		}
		domain := string(body[:domainLen])
		port := int(body[domainLen])<<8 | int(body[domainLen+1])
		target = net.JoinHostPort(domain, fmt.Sprintf("%d", port))

	case AddrIPv6:
		body := make([]byte, 16+2) // 16 bytes IPv6 + 2 bytes port
		if _, err := io.ReadFull(conn, body); err != nil {
			return "", fmt.Errorf("read IPv6 body: %w", err)
		}
		ip := net.IP(body[0:16]).String()
		port := int(body[16])<<8 | int(body[17])
		target = net.JoinHostPort(ip, fmt.Sprintf("%d", port))

	default:
		s.sendReply(conn, RepAddrNotSupported, nil)
		return "", fmt.Errorf("unsupported address type: %d", addrType)
	}

	return target, nil
}

// sendReply sends a SOCKS5 reply packet.
func (s *Server) sendReply(conn net.Conn, rep byte, bindAddr net.Addr) {
	reply := []byte{
		SocksVersion,
		rep,
		0x00, // Reserved
		AddrIPv4,
		0, 0, 0, 0, // Bind address (0.0.0.0)
		0, 0, // Bind port (0)
	}
	conn.Write(reply)
}
