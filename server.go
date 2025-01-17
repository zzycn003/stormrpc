package stormrpc

import (
	"context"
	"time"

	"github.com/nats-io/nats.go"
)

var defaultServerTimeout = 5 * time.Second

// Server represents a stormRPC server. It contains all functionality for handling RPC requests.
type Server struct {
	nc             *nats.Conn
	name           string
	shutdownSignal chan struct{}
	handlerFuncs   map[string]HandlerFunc
	errorHandler   ErrorHandler
	timeout        time.Duration
	mw             []Middleware
}

// NewServer returns a new instance of a Server.
func NewServer(name, natsURL string, opts ...ServerOption) (*Server, error) {
	options := serverOptions{
		errorHandler: func(ctx context.Context, err error) {},
	}

	for _, o := range opts {
		o.apply(&options)
	}

	nc, err := nats.Connect(natsURL)
	if err != nil {
		return nil, err
	}

	return &Server{
		nc:             nc,
		name:           name,
		shutdownSignal: make(chan struct{}),
		handlerFuncs:   make(map[string]HandlerFunc),
		timeout:        defaultServerTimeout,
		errorHandler:   options.errorHandler,
	}, nil
}

type serverOptions struct {
	errorHandler ErrorHandler
}

// ServerOption represents functional options for configuring a stormRPC Server.
type ServerOption interface {
	apply(*serverOptions)
}

type errorHandlerOption ErrorHandler

func (h errorHandlerOption) apply(opts *serverOptions) {
	opts.errorHandler = ErrorHandler(h)
}

// WithErrorHandler is a ServerOption that allows for registering a function for handling server errors.
func WithErrorHandler(fn ErrorHandler) ServerOption {
	return errorHandlerOption(fn)
}

// HandlerFunc is the function signature for handling of a single request to a stormRPC server.
type HandlerFunc func(ctx context.Context, r Request) Response

// Middleware is the function signature for wrapping HandlerFunc's to extend their functionality.
type Middleware func(next HandlerFunc) HandlerFunc

// ErrorHandler is the function signature for handling server errors.
type ErrorHandler func(context.Context, error)

// Handle registers a new HandlerFunc on the server.
func (s *Server) Handle(subject string, fn HandlerFunc) {
	s.handlerFuncs[subject] = fn
}

// Run listens on the configured subjects.
func (s *Server) Run() error {
	s.applyMiddlewares()
	for k := range s.handlerFuncs {
		_, err := s.nc.QueueSubscribe(k, s.name, s.handler)
		if err != nil {
			return err
		}
	}

	<-s.shutdownSignal
	return nil
}

// Shutdown stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.nc.FlushWithContext(ctx); err != nil {
		return err
	}

	s.nc.Close()
	s.shutdownSignal <- struct{}{}
	return nil
}

// Subjects returns a list of all subjects with registered handler funcs.
func (s *Server) Subjects() []string {
	subs := make([]string, 0, len(s.handlerFuncs))
	for k := range s.handlerFuncs {
		subs = append(subs, k)
	}

	return subs
}

// Use applies all given middleware globally across all handlers.
func (s *Server) Use(mw ...Middleware) {
	s.mw = mw
}

func (s *Server) applyMiddlewares() {
	for k, hf := range s.handlerFuncs {
		for i := len(s.mw) - 1; i >= 0; i-- {
			hf = s.mw[i](hf)
		}

		s.handlerFuncs[k] = hf
	}
}

// handler serves the request to the specific request handler based on subject.
// wildcard subjects are not supported as you'll need to register a handler func for each
// rpc the server supports.
func (s *Server) handler(msg *nats.Msg) {
	fn := s.handlerFuncs[msg.Subject]

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	dl := parseDeadlineHeader(msg.Header)
	if !dl.IsZero() { // if deadline is present use it
		ctx, cancel = context.WithDeadline(context.Background(), dl)
		defer cancel()
	}

	req := Request{
		Msg: msg,
	}
	ctx = newContextWithHeaders(ctx, req.Header)

	resp := fn(ctx, req)

	if resp.Err != nil {
		if resp.Header == nil {
			resp.Header = nats.Header{}
		}
		resp.Header.Set(errorHeader, resp.Err.Error())
		err := msg.RespondMsg(resp.Msg)
		if err != nil {
			s.errorHandler(ctx, err)
		}
	}

	err := msg.RespondMsg(resp.Msg)
	if err != nil {
		s.errorHandler(ctx, err)
	}
}
