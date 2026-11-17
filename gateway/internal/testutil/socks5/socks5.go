// Package socks5 is a minimal SOCKS5 server for tests (CONNECT only, optional
// username/password). It records which destinations clients asked for, so
// tests can assert that hostnames reach the proxy unresolved (remote DNS).
package socks5

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
)

// Server is a running test proxy.
type Server struct {
	ln       net.Listener
	user     string
	pass     string
	mu       sync.Mutex
	targets  []string // "host:port" as requested by clients
	conns    int
	closed   bool
	redirect func(target string) string // optional: rewrite the dial target (e.g. to a test upstream)
}

// Start listens on 127.0.0.1. user/pass empty = no authentication.
func Start(user, pass string) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, user: user, pass: pass}
	go s.serve()
	return s, nil
}

// Addr is "127.0.0.1:port".
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Redirect makes the proxy connect somewhere else than requested (tests point
// "api.tavily.com:443" at a local fake upstream).
func (s *Server) Redirect(fn func(target string) string) {
	s.mu.Lock()
	s.redirect = fn
	s.mu.Unlock()
}

// Targets returns the destinations clients requested, in order.
func (s *Server) Targets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.targets...)
}

// Conns is the number of accepted client connections.
func (s *Server) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

// Close stops the proxy; later dials to it fail (an egress outage).
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.ln.Close()
}

func (s *Server) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns++
		s.mu.Unlock()
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	target, err := s.handshake(c)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.targets = append(s.targets, target)
	dial := target
	if s.redirect != nil {
		dial = s.redirect(target)
	}
	s.mu.Unlock()
	up, err := net.Dial("tcp", dial)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}) // connection refused
		return
	}
	defer up.Close()
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func (s *Server) handshake(c net.Conn) (string, error) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return "", errors.New("not socks5")
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", err
	}
	want := byte(0)
	if s.user != "" {
		want = 2
	}
	ok := false
	for _, m := range methods {
		ok = ok || m == want
	}
	if !ok {
		_, _ = c.Write([]byte{5, 0xff})
		return "", errors.New("no acceptable auth method")
	}
	if _, err := c.Write([]byte{5, want}); err != nil {
		return "", err
	}
	if want == 2 {
		b := make([]byte, 2)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", err
		}
		u := make([]byte, b[1])
		if _, err := io.ReadFull(c, u); err != nil {
			return "", err
		}
		pl := make([]byte, 1)
		if _, err := io.ReadFull(c, pl); err != nil {
			return "", err
		}
		p := make([]byte, pl[0])
		if _, err := io.ReadFull(c, p); err != nil {
			return "", err
		}
		if string(u) != s.user || string(p) != s.pass {
			_, _ = c.Write([]byte{1, 1})
			return "", errors.New("bad credentials")
		}
		if _, err := c.Write([]byte{1, 0}); err != nil {
			return "", err
		}
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[1] != 1 {
		return "", errors.New("only CONNECT is supported")
	}
	var host string
	switch req[3] {
	case 1:
		ip := make([]byte, 4)
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return "", err
		}
		h := make([]byte, l[0])
		if _, err := io.ReadFull(c, h); err != nil {
			return "", err
		}
		host = string(h)
	case 4:
		ip := make([]byte, 16)
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	default:
		return "", errors.New("bad address type")
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb)))), nil
}
