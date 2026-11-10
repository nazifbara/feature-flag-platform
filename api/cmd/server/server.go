package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/nazifbara/flagsvc/api/internal/authservice"
	"github.com/nazifbara/flagsvc/api/internal/flagservice"
	"github.com/nazifbara/flagsvc/api/internal/interceptor"
	"github.com/nazifbara/flagsvc/api/internal/pingservice"
	"github.com/nazifbara/flagsvc/api/internal/store"
	"github.com/nazifbara/flagsvc/gen/go/flagsvc/auth/v1/authconnect"
	"github.com/nazifbara/flagsvc/gen/go/flagsvc/flag/v1/flagconnect"
	"github.com/nazifbara/flagsvc/gen/go/flagsvc/ping/v1/pingconnect"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/validate"
)

type server struct {
	httpServer *http.Server
	cancel     context.CancelFunc
}

// start listens on the server's address and serves until the server is shut
// down, which isn't reported as an error.
func (s *server) start() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}

	log.Printf("server running http://localhost:%d", ln.Addr().(*net.TCPAddr).Port)
	if err := s.httpServer.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// shutdown stops the server gracefully, waiting for open requests until ctx is
// done.
func (s *server) shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// newServer builds a server with the auth, ping and flag services registered,
// without starting it.
func newServer(port int, store *store.Store, cancel context.CancelFunc) *server {
	connectServer := connect.NewServer(
		interceptor.NewLoggingInterceptor(),
		validate.NewServerInterceptor(),
	)
	authconnect.RegisterAuthServiceHandler(
		connectServer,
		authservice.New(store),
	)
	pingconnect.RegisterPingServiceHandler(
		connectServer,
		pingservice.PingService{},
	)
	flagconnect.RegisterFlagServiceHandler(connectServer, flagservice.New(store))

	mux := http.NewServeMux()
	connecthttp.Mount(mux, connectServer)
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 5 * time.Second,
		Protocols:         p,
	}
	s := &server{
		httpServer: srv,
		cancel:     cancel,
	}
	return s
}

// withCORS lets the local frontend call h, and answers preflight requests itself.
func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:5173")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms")
		w.Header().Set("Access-Control-Expose-Headers", "Grpc-Status, Grpc-Message")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
