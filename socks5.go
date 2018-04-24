package socks5

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/net/context"
)

const (
	socks5Version = uint8(5)
)

// Config is used to setup and configure a Server
type Config struct {
	// AuthMethods can be provided to implement custom authentication
	// By default, "auth-less" mode is enabled.
	// For password-based auth use UserPassAuthenticator.
	AuthMethods []Authenticator

	// If provided, username/password authentication is enabled,
	// by appending a UserPassAuthenticator to AuthMethods. If not provided,
	// and AUthMethods is nil, then "auth-less" mode is enabled.
	Credentials CredentialStore

	// Resolver can be provided to do custom name resolution.
	// Defaults to DNSResolver if not provided.
	Resolver NameResolver

	// Rules is provided to enable custom logic around permitting
	// various commands. If not provided, PermitAll is used.
	Rules RuleSet

	// Rewriter can be used to transparently rewrite addresses.
	// This is invoked before the RuleSet is invoked.
	// Defaults to NoRewrite.
	Rewriter AddressRewriter

	// BindIP is used for bind or udp associate
	BindIP net.IP

	// Logger can be used to provide a custom log target.
	// Defaults to stdout.
	Logger *log.Logger

	// Optional function for dialing out
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	ConnLimit      int
	IdleTimeout    time.Duration
	ConnectTimeout time.Duration
}

// FinishedConnInfo contains information about finished connection
type FinishedConnInfo struct {
	IP       string
	Port     string
	Duration time.Duration
}

// Server is reponsible for accepting connections and handling
// the details of the SOCKS5 protocol
type Server struct {
	config           *Config
	authMethods      map[uint8]Authenticator
	sema             chan struct{}
	ConnCountChan    chan int64
	ConnCount        int64
	FinishedConnChan chan FinishedConnInfo
}

// New creates a new Server and potentially returns an error
func New(conf *Config) (*Server, error) {
	// Ensure we have at least one authentication method enabled
	if len(conf.AuthMethods) == 0 {
		if conf.Credentials != nil {
			conf.AuthMethods = []Authenticator{&UserPassAuthenticator{conf.Credentials}}
		} else {
			conf.AuthMethods = []Authenticator{&NoAuthAuthenticator{}}
		}
	}

	// Ensure we have a DNS resolver
	if conf.Resolver == nil {
		conf.Resolver = DNSResolver{}
	}

	// Ensure we have a rule set
	if conf.Rules == nil {
		conf.Rules = PermitAll()
	}

	// Ensure we have a log target
	if conf.Logger == nil {
		conf.Logger = log.New(os.Stdout, "", log.LstdFlags)
	}

	if conf.ConnLimit == 0 {
		conf.ConnLimit = 50000
	}
	server := &Server{
		config:           conf,
		sema:             make(chan struct{}, conf.ConnLimit),
		ConnCountChan:    make(chan int64),
		FinishedConnChan: make(chan FinishedConnInfo),
	}

	server.authMethods = make(map[uint8]Authenticator)

	for _, a := range conf.AuthMethods {
		server.authMethods[a.GetCode()] = a
	}

	return server, nil
}

// ListenAndServe is used to create a listener and serve on it
func (s *Server) ListenAndServe(network, addr string) {
	l, err := net.Listen(network, addr)
	if err != nil {
		return
	}
	s.Serve(l)
}

// GetConnCount returns connection count
func (s *Server) GetConnCount() int64 {
	return atomic.LoadInt64(&s.ConnCount)
}

// GetConnCountChan returns channel where every change in conn count is pushed to
func (s *Server) GetConnCountChan() chan int64 {
	return s.ConnCountChan
}

// GetFinishedConnChan returns channel where every finished conn info is pushed to
func (s *Server) GetFinishedConnChan() chan FinishedConnInfo {
	return s.FinishedConnChan
}

// Serve is used to serve connections from a listener
func (s *Server) Serve(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			s.config.Logger.Printf("[ERR] socks: %v", err)
		} else {
			conn.SetDeadline(time.Now().Add(s.config.ConnectTimeout))
			go s.ServeConn(conn)
		}
	}
}

// ServeConn is used to serve a single connection.
func (s *Server) ServeConn(conn net.Conn) error {
	defer func() {
		if r := recover(); r != nil {
			s.config.Logger.Printf("[ERR] socks: Panic recovered: %v", r)
		}
	}()
	defer conn.Close()
	select {
	case s.sema <- struct{}{}:
	default:
		err := fmt.Errorf("Failed to handle request: exhausted")
		s.config.Logger.Printf("[ERR] socks: %v", err)
		return err
	}
	defer func() {
		<-s.sema
		atomic.AddInt64(&s.ConnCount, -1)
		select {
		case s.ConnCountChan <- s.GetConnCount():
		default:
		}
	}()
	atomic.AddInt64(&s.ConnCount, 1)
	select {
	case s.ConnCountChan <- s.GetConnCount():
	default:
	}

	bufConn := bufio.NewReader(conn)

	// Read the version byte
	version := []byte{0}
	if _, err := bufConn.Read(version); err != nil {
		s.config.Logger.Printf("[ERR] socks: Failed to get version byte: %v", err)
		return err
	}

	// Ensure we are compatible
	if version[0] != socks5Version {
		err := fmt.Errorf("Unsupported SOCKS version: %v", version)
		s.config.Logger.Printf("[ERR] socks: %v", err)
		return err
	}

	// Authenticate the connection
	authContext, err := s.authenticate(conn, bufConn)
	if err != nil {
		err = fmt.Errorf("Failed to authenticate: %v", err)
		s.config.Logger.Printf("[ERR] socks: %v", err)
		return err
	}

	request, err := NewRequest(bufConn)
	if err != nil {
		if err == unrecognizedAddrType {
			if err := sendReply(conn, addrTypeNotSupported, nil); err != nil {
				return fmt.Errorf("Failed to send reply: %v", err)
			}
		}
		return fmt.Errorf("Failed to read destination address: %v", err)
	}
	request.AuthContext = authContext
	if client, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		request.RemoteAddr = &AddrSpec{IP: client.IP, Port: client.Port}
	}

	// Process the client request
	if err := s.handleRequest(request, conn); err != nil {
		err = fmt.Errorf("Failed to handle request: %v", err)
		s.config.Logger.Printf("[ERR] socks: %v", err)
		return err
	}

	return nil
}
