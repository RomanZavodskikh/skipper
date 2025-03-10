package proxy

import (
	"bytes"
	stdlibcontext "context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	ot "github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/zalando/skipper/circuit"
	"github.com/zalando/skipper/eskip"
	"github.com/zalando/skipper/filters"
	al "github.com/zalando/skipper/filters/accesslog"
	circuitfilters "github.com/zalando/skipper/filters/circuit"
	flowidFilter "github.com/zalando/skipper/filters/flowid"
	ratelimitfilters "github.com/zalando/skipper/filters/ratelimit"
	tracingfilter "github.com/zalando/skipper/filters/tracing"
	"github.com/zalando/skipper/loadbalancer"
	"github.com/zalando/skipper/logging"
	"github.com/zalando/skipper/metrics"
	"github.com/zalando/skipper/proxy/fastcgi"
	"github.com/zalando/skipper/ratelimit"
	"github.com/zalando/skipper/rfc"
	"github.com/zalando/skipper/routing"
	"github.com/zalando/skipper/scheduler"
	"github.com/zalando/skipper/tracing"
)

const (
	proxyBufferSize         = 8192
	unknownRouteID          = "_unknownroute_"
	unknownRouteBackendType = "<unknown>"
	unknownRouteBackend     = "<unknown>"

	// Number of loops allowed by default.
	DefaultMaxLoopbacks = 9

	// The default value set for http.Transport.MaxIdleConnsPerHost.
	DefaultIdleConnsPerHost = 64

	// The default period at which the idle connections are forcibly
	// closed.
	DefaultCloseIdleConnsPeriod = 20 * time.Second

	// DefaultResponseHeaderTimeout, the default response header timeout
	DefaultResponseHeaderTimeout = 60 * time.Second

	// DefaultExpectContinueTimeout, the default timeout to expect
	// a response for a 100 Continue request
	DefaultExpectContinueTimeout = 30 * time.Second
)

// Flags control the behavior of the proxy.
type Flags uint

const (
	FlagsNone Flags = 0

	// Insecure causes the proxy to ignore the verification of
	// the TLS certificates of the backend services.
	Insecure Flags = 1 << iota

	// PreserveOriginal indicates that filters require the
	// preserved original metadata of the request and the response.
	PreserveOriginal

	// PreserveHost indicates whether the outgoing request to the
	// backend should use by default the 'Host' header of the incoming
	// request, or the host part of the backend address, in case filters
	// don't change it.
	PreserveHost

	// Debug indicates that the current proxy instance will be used as a
	// debug proxy. Debug proxies don't forward the request to the
	// route backends, but they execute all filters, and return a
	// JSON document with the changes the filters make to the request
	// and with the approximate changes they would make to the
	// response.
	Debug

	// HopHeadersRemoval indicates whether the Hop Headers should be removed
	// in compliance with RFC 2616
	HopHeadersRemoval

	// PatchPath instructs the proxy to patch the parsed request path
	// if the reserved characters according to RFC 2616 and RFC 3986
	// were unescaped by the parser.
	PatchPath
)

// Options are deprecated alias for Flags.
type Options Flags

const (
	OptionsNone              = Options(FlagsNone)
	OptionsInsecure          = Options(Insecure)
	OptionsPreserveOriginal  = Options(PreserveOriginal)
	OptionsPreserveHost      = Options(PreserveHost)
	OptionsDebug             = Options(Debug)
	OptionsHopHeadersRemoval = Options(HopHeadersRemoval)
)

type OpenTracingParams struct {
	// Tracer holds the tracer enabled for this proxy instance
	Tracer ot.Tracer

	// InitialSpan can override the default initial, pre-routing, span name.
	// Default: "ingress".
	InitialSpan string

	// DisableFilterSpans disables creation of spans representing request and response filters.
	// Default: false
	DisableFilterSpans bool

	// LogFilterEvents enables the behavior to mark start and completion times of filters
	// on the span representing request/response filters being processed.
	// Default: false
	LogFilterEvents bool

	// LogStreamEvents enables the logs that marks the times when response headers & payload are streamed to
	// the client
	// Default: false
	LogStreamEvents bool

	// ExcludeTags controls what tags are disabled. Any tag that is listed here will be ignored.
	ExcludeTags []string
}

// Proxy initialization options.
type Params struct {
	// The proxy expects a routing instance that is used to match
	// the incoming requests to routes.
	Routing *routing.Routing

	// Control flags. See the Flags values.
	Flags Flags

	// And optional list of priority routes to be used for matching
	// before the general lookup tree.
	PriorityRoutes []PriorityRoute

	// Enable the experimental upgrade protocol feature
	ExperimentalUpgrade bool

	// ExperimentalUpgradeAudit enables audit log of both the request line
	// and the response messages during web socket upgrades.
	ExperimentalUpgradeAudit bool

	// When set, no access log is printed.
	AccessLogDisabled bool

	// DualStack sets if the proxy TCP connections to the backend should be dual stack
	DualStack bool

	// DefaultHTTPStatus is the HTTP status used when no routes are found
	// for a request.
	DefaultHTTPStatus int

	// MaxLoopbacks sets the maximum number of allowed loops. If 0
	// the default (9) is applied. To disable looping, set it to
	// -1. Note, that disabling looping by this option, may result
	// wrong routing depending on the current configuration.
	MaxLoopbacks int

	// Same as net/http.Transport.MaxIdleConnsPerHost, but the default
	// is 64. This value supports scenarios with relatively few remote
	// hosts. When the routing table contains different hosts in the
	// range of hundreds, it is recommended to set this options to a
	// lower value.
	IdleConnectionsPerHost int

	// MaxIdleConns limits the number of idle connections to all backends, 0 means no limit
	MaxIdleConns int

	// DisableHTTPKeepalives forces backend to always create a new connection
	DisableHTTPKeepalives bool

	// CircuitBreakers provides a registry that skipper can use to
	// find the matching circuit breaker for backend requests. If not
	// set, no circuit breakers are used.
	CircuitBreakers *circuit.Registry

	// RateLimiters provides a registry that skipper can use to
	// find the matching ratelimiter for backend requests. If not
	// set, no ratelimits are used.
	RateLimiters *ratelimit.Registry

	// LoadBalancer to report unhealthy or dead backends to
	LoadBalancer *loadbalancer.LB

	// Defines the time period of how often the idle connections are
	// forcibly closed. The default is 12 seconds. When set to less than
	// 0, the proxy doesn't force closing the idle connections.
	CloseIdleConnsPeriod time.Duration

	// The Flush interval for copying upgraded connections
	FlushInterval time.Duration

	// Timeout sets the TCP client connection timeout for proxy http connections to the backend
	Timeout time.Duration

	// ResponseHeaderTimeout sets the HTTP response timeout for
	// proxy http connections to the backend.
	ResponseHeaderTimeout time.Duration

	// ExpectContinueTimeout sets the HTTP timeout to expect a
	// response for status Code 100 for proxy http connections to
	// the backend.
	ExpectContinueTimeout time.Duration

	// KeepAlive sets the TCP keepalive for proxy http connections to the backend
	KeepAlive time.Duration

	// TLSHandshakeTimeout sets the TLS handshake timeout for proxy connections to the backend
	TLSHandshakeTimeout time.Duration

	// Client TLS to connect to Backends
	ClientTLS *tls.Config

	// OpenTracing contains parameters related to OpenTracing instrumentation. For default values
	// check OpenTracingParams
	OpenTracing *OpenTracingParams

	// CustomHttpRoundTripperWrap provides ability to wrap http.RoundTripper created by skipper.
	// http.RoundTripper is used for making outgoing requests (backends)
	// It allows to add additional logic (for example tracing) by providing a wrapper function
	// which accepts original skipper http.RoundTripper as an argument and returns a wrapped roundtripper
	CustomHttpRoundTripperWrap func(http.RoundTripper) http.RoundTripper
}

type (
	maxLoopbackError string
	ratelimitError   string
	routeLookupError string
)

func (e maxLoopbackError) Error() string { return string(e) }
func (e ratelimitError) Error() string   { return string(e) }
func (e routeLookupError) Error() string { return string(e) }

const (
	errMaxLoopbacksReached = maxLoopbackError("max loopbacks reached")
	errRatelimit           = ratelimitError("ratelimited")
	errRouteLookup         = routeLookupError("route lookup failed")
)

var (
	errRouteLookupFailed  = &proxyError{err: errRouteLookup}
	errCircuitBreakerOpen = &proxyError{
		err:              errors.New("circuit breaker open"),
		code:             http.StatusServiceUnavailable,
		additionalHeader: http.Header{"X-Circuit-Open": []string{"true"}},
	}

	disabledAccessLog = al.AccessLogFilter{Enable: false, Prefixes: nil}
	enabledAccessLog  = al.AccessLogFilter{Enable: true, Prefixes: nil}
	hopHeaders        = map[string]bool{
		"Te":                  true,
		"Connection":          true,
		"Proxy-Connection":    true,
		"Keep-Alive":          true,
		"Proxy-Authenticate":  true,
		"Proxy-Authorization": true,
		"Trailer":             true,
		"Transfer-Encoding":   true,
		"Upgrade":             true,
	}
)

// When set, the proxy will skip the TLS verification on outgoing requests.
func (f Flags) Insecure() bool { return f&Insecure != 0 }

// When set, the filters will receive an unmodified clone of the original
// incoming request and response.
func (f Flags) PreserveOriginal() bool { return f&(PreserveOriginal|Debug) != 0 }

// When set, the proxy will set the, by default, the Host header value
// of the outgoing requests to the one of the incoming request.
func (f Flags) PreserveHost() bool { return f&PreserveHost != 0 }

// When set, the proxy runs in debug mode.
func (f Flags) Debug() bool { return f&Debug != 0 }

// When set, the proxy will remove the Hop Headers
func (f Flags) HopHeadersRemoval() bool { return f&HopHeadersRemoval != 0 }

func (f Flags) patchPath() bool { return f&PatchPath != 0 }

// Priority routes are custom route implementations that are matched against
// each request before the routes in the general lookup tree.
type PriorityRoute interface {
	// If the request is matched, returns a route, otherwise nil.
	// Additionally it may return a parameter map used by the filters
	// in the route.
	Match(*http.Request) (*routing.Route, map[string]string)
}

// Proxy instances implement Skipper proxying functionality. For
// initializing, see the WithParams the constructor and Params.
type Proxy struct {
	experimentalUpgrade      bool
	experimentalUpgradeAudit bool
	accessLogDisabled        bool
	maxLoops                 int
	defaultHTTPStatus        int
	routing                  *routing.Routing
	roundTripper             http.RoundTripper
	priorityRoutes           []PriorityRoute
	flags                    Flags
	metrics                  metrics.Metrics
	quit                     chan struct{}
	flushInterval            time.Duration
	breakers                 *circuit.Registry
	limiters                 *ratelimit.Registry
	log                      logging.Logger
	tracing                  *proxyTracing
	lb                       *loadbalancer.LB
	upgradeAuditLogOut       io.Writer
	upgradeAuditLogErr       io.Writer
	auditLogHook             chan struct{}
	clientTLS                *tls.Config
	hostname                 string
}

// proxyError is used to wrap errors during proxying and to indicate
// the required status code for the response sent from the main
// ServeHTTP method. Alternatively, it can indicate that the request
// was already handled, e.g. in case of deprecated shunting or the
// upgrade request.
type proxyError struct {
	err              error
	code             int
	handled          bool
	dialingFailed    bool
	additionalHeader http.Header
}

func (e proxyError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("dialing failed %v: %v", e.DialError(), e.err.Error())
	}

	if e.handled {
		return "request handled in a non-standard way"
	}

	code := e.code
	if code == 0 {
		code = http.StatusInternalServerError
	}

	return fmt.Sprintf("proxy error: %d", code)
}

// DialError returns true if the error was caused while dialing TCP or
// TLS connections, before HTTP data was sent. It is safe to retry
// a call, if this returns true.
func (e *proxyError) DialError() bool {
	return e.dialingFailed
}

func copyHeader(to, from http.Header) {
	for k, v := range from {
		to[http.CanonicalHeaderKey(k)] = v
	}
}

func copyHeaderExcluding(to, from http.Header, excludeHeaders map[string]bool) {
	for k, v := range from {
		// The http package converts header names to their canonical version.
		// Meaning that the lookup below will be done using the canonical version of the header.
		if _, ok := excludeHeaders[k]; !ok {
			to[http.CanonicalHeaderKey(k)] = v
		}
	}
}

func cloneHeader(h http.Header) http.Header {
	hh := make(http.Header)
	copyHeader(hh, h)
	return hh
}

func cloneHeaderExcluding(h http.Header, excludeList map[string]bool) http.Header {
	hh := make(http.Header)
	copyHeaderExcluding(hh, h, excludeList)
	return hh
}

type flusher struct {
	w flushedResponseWriter
}

func (f *flusher) Write(p []byte) (n int, err error) {
	n, err = f.w.Write(p)
	if err == nil {
		f.w.Flush()
	}
	return
}

func copyStream(to flushedResponseWriter, from io.Reader) (int64, error) {
	b := make([]byte, proxyBufferSize)

	return io.CopyBuffer(&flusher{to}, from, b)
}

func schemeFromRequest(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func setRequestURLFromRequest(u *url.URL, r *http.Request) {
	if u.Host == "" {
		u.Host = r.Host
	}
	if u.Scheme == "" {
		u.Scheme = schemeFromRequest(r)
	}
}

func setRequestURLForDynamicBackend(u *url.URL, stateBag map[string]interface{}) {
	dbu, ok := stateBag[filters.DynamicBackendURLKey].(string)
	if ok && dbu != "" {
		bu, err := url.ParseRequestURI(dbu)
		if err == nil {
			u.Host = bu.Host
			u.Scheme = bu.Scheme
		}
	} else {
		host, ok := stateBag[filters.DynamicBackendHostKey].(string)
		if ok && host != "" {
			u.Host = host
		}

		scheme, ok := stateBag[filters.DynamicBackendSchemeKey].(string)
		if ok && scheme != "" {
			u.Scheme = scheme
		}
	}
}

func setRequestURLForLoadBalancedBackend(u *url.URL, rt *routing.Route, lbctx *routing.LBContext) *routing.LBEndpoint {
	e := rt.LBAlgorithm.Apply(lbctx)
	u.Scheme = e.Scheme
	u.Host = e.Host
	return &e
}

// creates an outgoing http request to be forwarded to the route endpoint
// based on the augmented incoming request
func mapRequest(ctx *context, requestContext stdlibcontext.Context, removeHopHeaders bool) (*http.Request, *routing.LBEndpoint, error) {
	var endpoint *routing.LBEndpoint
	r := ctx.request
	rt := ctx.route
	host := ctx.outgoingHost
	stateBag := ctx.StateBag()
	u := r.URL

	switch rt.BackendType {
	case eskip.DynamicBackend:
		setRequestURLFromRequest(u, r)
		setRequestURLForDynamicBackend(u, stateBag)
	case eskip.LBBackend:
		endpoint = setRequestURLForLoadBalancedBackend(u, rt, &routing.LBContext{Request: r, Route: rt, Params: stateBag})
	default:
		u.Scheme = rt.Scheme
		u.Host = rt.Host
	}

	body := r.Body
	if r.ContentLength == 0 {
		body = nil
	}

	rr, err := http.NewRequestWithContext(requestContext, r.Method, u.String(), body)
	if err != nil {
		return nil, endpoint, err
	}

	rr.ContentLength = r.ContentLength
	if removeHopHeaders {
		rr.Header = cloneHeaderExcluding(r.Header, hopHeaders)
	} else {
		rr.Header = cloneHeader(r.Header)
	}
	// Disable default net/http user agent when user agent is not specified
	if _, ok := rr.Header["User-Agent"]; !ok {
		rr.Header["User-Agent"] = []string{""}
	}
	rr.Host = host

	// If there is basic auth configured in the URL we add them as headers
	if u.User != nil {
		up := u.User.String()
		upBase64 := base64.StdEncoding.EncodeToString([]byte(up))
		rr.Header.Add("Authorization", fmt.Sprintf("Basic %s", upBase64))
	}

	ctxspan := ot.SpanFromContext(r.Context())
	if ctxspan != nil {
		rr = rr.WithContext(ot.ContextWithSpan(rr.Context(), ctxspan))
	}

	if _, ok := stateBag[filters.BackendIsProxyKey]; ok {
		rr = forwardToProxy(r, rr)
	}

	return rr, endpoint, nil
}

type proxyUrlContextKey struct{}

func forwardToProxy(incoming, outgoing *http.Request) *http.Request {
	proxyURL := &url.URL{
		Scheme: outgoing.URL.Scheme,
		Host:   outgoing.URL.Host,
	}

	outgoing.URL.Host = incoming.Host
	outgoing.URL.Scheme = schemeFromRequest(incoming)

	return outgoing.WithContext(stdlibcontext.WithValue(outgoing.Context(), proxyUrlContextKey{}, proxyURL))
}

func proxyFromContext(req *http.Request) (*url.URL, error) {
	proxyURL, _ := req.Context().Value(proxyUrlContextKey{}).(*url.URL)
	if proxyURL != nil {
		return proxyURL, nil
	}
	return nil, nil
}

type skipperDialer struct {
	net.Dialer
	f func(ctx stdlibcontext.Context, network, addr string) (net.Conn, error)
}

func newSkipperDialer(d net.Dialer) *skipperDialer {
	return &skipperDialer{
		Dialer: d,
		f:      d.DialContext,
	}
}

// DialContext wraps net.Dialer's DialContext and returns an error,
// that can be checked if it was a Transport (TCP/TLS handshake) error
// or timeout, or a timeout from http, which is not in general
// not possible to retry.
func (dc *skipperDialer) DialContext(ctx stdlibcontext.Context, network, addr string) (net.Conn, error) {
	span := ot.SpanFromContext(ctx)
	if span != nil {
		span.LogKV("dial_context", "start")
	}
	con, err := dc.f(ctx, network, addr)
	if span != nil {
		span.LogKV("dial_context", "done")
	}
	if err != nil {
		return nil, &proxyError{
			err:           err,
			code:          -1,   // omit 0 handling in proxy.Error()
			dialingFailed: true, // indicate error happened before http
		}
	} else if cerr := ctx.Err(); cerr != nil {
		// unclear when this is being triggered
		return nil, &proxyError{
			err:  fmt.Errorf("err from dial context: %w", cerr),
			code: http.StatusGatewayTimeout,
		}
	}
	return con, nil
}

// New returns an initialized Proxy.
// Deprecated, see WithParams and Params instead.
func New(r *routing.Routing, options Options, pr ...PriorityRoute) *Proxy {
	return WithParams(Params{
		Routing:              r,
		Flags:                Flags(options),
		PriorityRoutes:       pr,
		CloseIdleConnsPeriod: -time.Second,
	})
}

// WithParams returns an initialized Proxy.
func WithParams(p Params) *Proxy {
	if p.IdleConnectionsPerHost <= 0 {
		p.IdleConnectionsPerHost = DefaultIdleConnsPerHost
	}

	if p.CloseIdleConnsPeriod == 0 {
		p.CloseIdleConnsPeriod = DefaultCloseIdleConnsPeriod
	}

	if p.ResponseHeaderTimeout == 0 {
		p.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	}

	if p.ExpectContinueTimeout == 0 {
		p.ExpectContinueTimeout = DefaultExpectContinueTimeout
	}

	if p.CustomHttpRoundTripperWrap == nil {
		// default wrapper which does nothing
		p.CustomHttpRoundTripperWrap = func(original http.RoundTripper) http.RoundTripper {
			return original
		}
	}

	tr := &http.Transport{
		DialContext: newSkipperDialer(net.Dialer{
			Timeout:   p.Timeout,
			KeepAlive: p.KeepAlive,
			DualStack: p.DualStack,
		}).DialContext,
		TLSHandshakeTimeout:   p.TLSHandshakeTimeout,
		ResponseHeaderTimeout: p.ResponseHeaderTimeout,
		ExpectContinueTimeout: p.ExpectContinueTimeout,
		MaxIdleConns:          p.MaxIdleConns,
		MaxIdleConnsPerHost:   p.IdleConnectionsPerHost,
		IdleConnTimeout:       p.CloseIdleConnsPeriod,
		DisableKeepAlives:     p.DisableHTTPKeepalives,
		Proxy:                 proxyFromContext,
	}

	quit := make(chan struct{})
	// We need this to reliably fade on DNS change, which is right
	// now not fixed with IdleConnTimeout in the http.Transport.
	// https://github.com/golang/go/issues/23427
	if p.CloseIdleConnsPeriod > 0 {
		go func() {
			for {
				select {
				case <-time.After(p.CloseIdleConnsPeriod):
					tr.CloseIdleConnections()
				case <-quit:
					return
				}
			}
		}()
	}

	if p.ClientTLS != nil {
		tr.TLSClientConfig = p.ClientTLS
	}

	if p.Flags.Insecure() {
		if tr.TLSClientConfig == nil {
			/* #nosec */
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		} else {
			/* #nosec */
			tr.TLSClientConfig.InsecureSkipVerify = true
		}
	}

	m := metrics.Default
	if p.Flags.Debug() {
		m = metrics.Void
	}

	if p.MaxLoopbacks == 0 {
		p.MaxLoopbacks = DefaultMaxLoopbacks
	} else if p.MaxLoopbacks < 0 {
		p.MaxLoopbacks = 0
	}

	defaultHTTPStatus := http.StatusNotFound

	if p.DefaultHTTPStatus >= http.StatusContinue && p.DefaultHTTPStatus <= http.StatusNetworkAuthenticationRequired {
		defaultHTTPStatus = p.DefaultHTTPStatus
	}

	hostname := os.Getenv("HOSTNAME")

	return &Proxy{
		routing:                  p.Routing,
		roundTripper:             p.CustomHttpRoundTripperWrap(tr),
		priorityRoutes:           p.PriorityRoutes,
		flags:                    p.Flags,
		metrics:                  m,
		quit:                     quit,
		flushInterval:            p.FlushInterval,
		experimentalUpgrade:      p.ExperimentalUpgrade,
		experimentalUpgradeAudit: p.ExperimentalUpgradeAudit,
		maxLoops:                 p.MaxLoopbacks,
		breakers:                 p.CircuitBreakers,
		lb:                       p.LoadBalancer,
		limiters:                 p.RateLimiters,
		log:                      &logging.DefaultLog{},
		defaultHTTPStatus:        defaultHTTPStatus,
		tracing:                  newProxyTracing(p.OpenTracing),
		accessLogDisabled:        p.AccessLogDisabled,
		upgradeAuditLogOut:       os.Stdout,
		upgradeAuditLogErr:       os.Stderr,
		clientTLS:                tr.TLSClientConfig,
		hostname:                 hostname,
	}
}

var caughtPanic = false

// tryCatch executes function `p` and `onErr` if `p` panics
// onErr will receive a stack trace string of the first panic
// further panics are ignored for efficiency reasons
func tryCatch(p func(), onErr func(err interface{}, stack string)) {
	defer func() {
		if err := recover(); err != nil {
			s := ""
			if !caughtPanic {
				buf := make([]byte, 1024)
				l := runtime.Stack(buf, false)
				s = string(buf[:l])
				caughtPanic = true
			}
			onErr(err, s)
		}
	}()

	p()
}

// applies filters to a request
func (p *Proxy) applyFiltersToRequest(f []*routing.RouteFilter, ctx *context) []*routing.RouteFilter {
	if len(f) == 0 {
		return f
	}

	filtersStart := time.Now()
	filterTracing := p.tracing.startFilterTracing("request_filters", ctx)
	defer filterTracing.finish()

	var filters = make([]*routing.RouteFilter, 0, len(f))
	for _, fi := range f {
		start := time.Now()
		filterTracing.logStart(fi.Name)
		tryCatch(func() {
			ctx.setMetricsPrefix(fi.Name)
			fi.Request(ctx)
			p.metrics.MeasureFilterRequest(fi.Name, start)
		}, func(err interface{}, stack string) {
			if p.flags.Debug() {
				// these errors are collected for the debug mode to be able
				// to report in the response which filters failed.
				ctx.debugFilterPanics = append(ctx.debugFilterPanics, err)
				return
			}

			p.log.Errorf("error while processing filter during request: %s: %v (%s)", fi.Name, err, stack)
		})
		filterTracing.logEnd(fi.Name)

		filters = append(filters, fi)
		if ctx.deprecatedShunted() || ctx.shunted() {
			break
		}
	}

	p.metrics.MeasureAllFiltersRequest(ctx.route.Id, filtersStart)
	return filters
}

// applies filters to a response in reverse order
func (p *Proxy) applyFiltersToResponse(filters []*routing.RouteFilter, ctx *context) {
	filtersStart := time.Now()
	filterTracing := p.tracing.startFilterTracing("response_filters", ctx)
	defer filterTracing.finish()

	last := len(filters) - 1
	for i := range filters {
		fi := filters[last-i]
		start := time.Now()
		filterTracing.logStart(fi.Name)
		tryCatch(func() {
			ctx.setMetricsPrefix(fi.Name)
			fi.Response(ctx)
			p.metrics.MeasureFilterResponse(fi.Name, start)
		}, func(err interface{}, stack string) {
			if p.flags.Debug() {
				// these errors are collected for the debug mode to be able
				// to report in the response which filters failed.
				ctx.debugFilterPanics = append(ctx.debugFilterPanics, err)
				return
			}

			p.log.Errorf("error while processing filters during response: %s: %v (%s)", fi.Name, err, stack)
		})
		filterTracing.logEnd(fi.Name)
	}

	p.metrics.MeasureAllFiltersResponse(ctx.route.Id, filtersStart)
}

// addBranding overwrites any existing `X-Powered-By` or `Server` header from headerMap
func addBranding(headerMap http.Header) {
	if headerMap.Get("Server") == "" {
		headerMap.Set("Server", "Skipper")
	}
}

func (p *Proxy) lookupRoute(ctx *context) (rt *routing.Route, params map[string]string) {
	for _, prt := range p.priorityRoutes {
		rt, params = prt.Match(ctx.request)
		if rt != nil {
			return rt, params
		}
	}

	return ctx.routeLookup.Do(ctx.request)
}

// send a premature error response
func (p *Proxy) sendError(c *context, id string, code int) {
	addBranding(c.responseWriter.Header())

	text := http.StatusText(code) + "\n"

	c.responseWriter.Header().Set("Content-Length", strconv.Itoa(len(text)))
	c.responseWriter.Header().Set("Content-Type", "text/plain; charset=utf-8")
	c.responseWriter.Header().Set("X-Content-Type-Options", "nosniff")
	c.responseWriter.WriteHeader(code)
	c.responseWriter.Write([]byte(text))

	p.metrics.MeasureServe(
		id,
		c.metricsHost(),
		c.request.Method,
		code,
		c.startServe,
	)
}

func (p *Proxy) makeUpgradeRequest(ctx *context, req *http.Request) error {
	backendURL := req.URL

	reverseProxy := httputil.NewSingleHostReverseProxy(backendURL)
	reverseProxy.FlushInterval = p.flushInterval
	upgradeProxy := upgradeProxy{
		backendAddr:     backendURL,
		reverseProxy:    reverseProxy,
		insecure:        p.flags.Insecure(),
		tlsClientConfig: p.clientTLS,
		useAuditLog:     p.experimentalUpgradeAudit,
		auditLogOut:     p.upgradeAuditLogOut,
		auditLogErr:     p.upgradeAuditLogErr,
		auditLogHook:    p.auditLogHook,
	}

	upgradeProxy.serveHTTP(ctx.responseWriter, req)
	ctx.successfulUpgrade = true
	p.log.Debugf("finished upgraded protocol %s session", getUpgradeRequest(ctx.request))
	return nil
}

func (p *Proxy) makeBackendRequest(ctx *context, requestContext stdlibcontext.Context) (*http.Response, *proxyError) {
	req, endpoint, err := mapRequest(ctx, requestContext, p.flags.HopHeadersRemoval())
	if err != nil {
		return nil, &proxyError{err: fmt.Errorf("could not map backend request: %w", err)}
	}

	if res, ok := p.rejectBackend(ctx, req); ok {
		return res, nil
	}

	if endpoint != nil {
		endpoint.Metrics.IncInflightRequest()
		defer endpoint.Metrics.DecInflightRequest()
	}

	if p.experimentalUpgrade && isUpgradeRequest(req) {
		if err = p.makeUpgradeRequest(ctx, req); err != nil {
			return nil, &proxyError{err: err}
		}

		// We are not owner of the connection anymore.
		return nil, &proxyError{handled: true}
	}

	roundTripper, err := p.getRoundTripper(ctx, req)
	if err != nil {
		return nil, &proxyError{err: fmt.Errorf("failed to get roundtripper: %w", err), code: http.StatusBadGateway}
	}

	bag := ctx.StateBag()
	spanName, ok := bag[tracingfilter.OpenTracingProxySpanKey].(string)
	if !ok {
		spanName = "proxy"
	}
	ctx.proxySpan = tracing.CreateSpan(spanName, req.Context(), p.tracing.tracer)

	u := cloneURL(req.URL)
	u.RawQuery = ""
	p.tracing.
		setTag(ctx.proxySpan, SpanKindTag, SpanKindClient).
		setTag(ctx.proxySpan, SkipperRouteIDTag, ctx.route.Id).
		setTag(ctx.proxySpan, HTTPUrlTag, u.String())
	p.setCommonSpanInfo(u, req, ctx.proxySpan)

	carrier := ot.HTTPHeadersCarrier(req.Header)
	_ = p.tracing.tracer.Inject(ctx.proxySpan.Context(), ot.HTTPHeaders, carrier)

	req = req.WithContext(ot.ContextWithSpan(req.Context(), ctx.proxySpan))

	p.metrics.IncCounter("outgoing." + req.Proto)
	ctx.proxySpan.LogKV("http_roundtrip", StartEvent)
	req = injectClientTrace(req, ctx.proxySpan)

	response, err := roundTripper.RoundTrip(req)

	ctx.proxySpan.LogKV("http_roundtrip", EndEvent)
	if err != nil {
		p.tracing.setTag(ctx.proxySpan, ErrorTag, true)

		// Check if the request has been cancelled or timed out
		// The roundtrip error `err` may be different:
		// - for `Canceled` it could be either the same `context canceled` or `unexpected EOF` (net.OpError)
		// - for `DeadlineExceeded` it is net.Error(timeout=true, temporary=true) wrapping this `context deadline exceeded`
		if cerr := req.Context().Err(); cerr != nil {
			ctx.proxySpan.LogKV("event", "error", "message", cerr.Error())
			if cerr == stdlibcontext.Canceled {
				return nil, &proxyError{err: cerr, code: 499}
			} else if cerr == stdlibcontext.DeadlineExceeded {
				return nil, &proxyError{err: cerr, code: http.StatusGatewayTimeout}
			}
		}

		ctx.proxySpan.LogKV("event", "error", "message", err.Error())

		if perr, ok := err.(*proxyError); ok {
			//p.lb.AddHealthcheck(ctx.route.Backend)
			perr.err = fmt.Errorf("failed to do backend roundtrip to %s: %w", req.URL.Host, perr.err)
			return nil, perr

		} else if nerr, ok := err.(net.Error); ok {
			//p.lb.AddHealthcheck(ctx.route.Backend)
			var status int
			if nerr.Timeout() {
				status = http.StatusGatewayTimeout
			} else {
				status = http.StatusServiceUnavailable
			}
			p.tracing.setTag(ctx.proxySpan, HTTPStatusCodeTag, uint16(status))
			//lint:ignore SA1019 Temporary is deprecated in Go 1.18, but keep it for now (https://github.com/zalando/skipper/issues/1992)
			return nil, &proxyError{err: fmt.Errorf("net.Error during backend roundtrip to %s: timeout=%v temporary='%v': %w", req.URL.Host, nerr.Timeout(), nerr.Temporary(), err), code: status}
		}

		return nil, &proxyError{err: fmt.Errorf("unexpected error from Go stdlib net/http package during roundtrip: %w", err)}
	}
	p.tracing.setTag(ctx.proxySpan, HTTPStatusCodeTag, uint16(response.StatusCode))
	return response, nil
}

func (p *Proxy) getRoundTripper(ctx *context, req *http.Request) (http.RoundTripper, error) {
	switch req.URL.Scheme {
	case "fastcgi":
		f := "index.php"
		if sf, ok := ctx.StateBag()["fastCgiFilename"]; ok {
			f = sf.(string)
		} else if len(req.URL.Path) > 1 && req.URL.Path != "/" {
			f = req.URL.Path[1:]
		}
		rt, err := fastcgi.NewRoundTripper(p.log, req.URL.Host, f)
		if err != nil {
			return nil, err
		}

		// FastCgi expects the Host to be in form host:port
		// It will then be split and added as 2 separate params to the backend process
		if _, _, err := net.SplitHostPort(req.Host); err != nil {
			req.Host = req.Host + ":" + req.URL.Port()
		}

		// RemoteAddr is needed to pass to the backend process as param
		req.RemoteAddr = ctx.request.RemoteAddr

		return rt, nil
	default:
		return p.roundTripper, nil
	}
}

func (p *Proxy) rejectBackend(ctx *context, req *http.Request) (*http.Response, bool) {
	limit, ok := ctx.StateBag()[filters.BackendRatelimit].(*ratelimitfilters.BackendRatelimit)
	if ok {
		s := req.URL.Scheme + "://" + req.URL.Host

		if !p.limiters.Get(limit.Settings).AllowContext(req.Context(), s) {
			return &http.Response{
				StatusCode: limit.StatusCode,
				Header:     http.Header{"Content-Length": []string{"0"}},
				Body:       io.NopCloser(&bytes.Buffer{}),
			}, true
		}
	}
	return nil, false
}

func (p *Proxy) checkBreaker(c *context) (func(bool), bool) {
	if p.breakers == nil {
		return nil, true
	}

	settings, _ := c.stateBag[circuitfilters.RouteSettingsKey].(circuit.BreakerSettings)
	settings.Host = c.outgoingHost

	b := p.breakers.Get(settings)
	if b == nil {
		return nil, true
	}

	done, ok := b.Allow()
	if !ok && c.request.Body != nil {
		// consume the body to prevent goroutine leaks
		io.Copy(io.Discard, c.request.Body)
	}
	return done, ok
}

func newRatelimitError(settings ratelimit.Settings, retryAfter int) error {
	return &proxyError{
		err:              errRatelimit,
		code:             http.StatusTooManyRequests,
		additionalHeader: ratelimit.Headers(settings.MaxHits, settings.TimeWindow, retryAfter),
	}
}

func (p *Proxy) do(ctx *context) error {
	if ctx.executionCounter > p.maxLoops {
		return errMaxLoopbacksReached
	}

	defer func() {
		pendingLIFO, _ := ctx.StateBag()[scheduler.LIFOKey].([]func())
		for _, done := range pendingLIFO {
			done()
		}
	}()

	// proxy global setting
	if !ctx.wasExecuted() {
		if settings, retryAfter := p.limiters.Check(ctx.request); retryAfter > 0 {
			rerr := newRatelimitError(settings, retryAfter)
			return rerr
		}
	}
	// every time the context is used for a request the context executionCounter is incremented
	// a context executionCounter equal to zero represents a root context.
	ctx.executionCounter++
	lookupStart := time.Now()
	route, params := p.lookupRoute(ctx)
	p.metrics.MeasureRouteLookup(lookupStart)
	if route == nil {
		if !p.flags.Debug() {
			p.metrics.IncRoutingFailures()
		}

		p.log.Debugf("could not find a route for %v", ctx.request.URL)
		return errRouteLookupFailed
	}

	ctx.applyRoute(route, params, p.flags.PreserveHost())

	processedFilters := p.applyFiltersToRequest(ctx.route.Filters, ctx)

	if ctx.deprecatedShunted() {
		p.log.Debugf("deprecated shunting detected in route: %s", ctx.route.Id)
		return &proxyError{handled: true}
	} else if ctx.shunted() || ctx.route.Shunt || ctx.route.BackendType == eskip.ShuntBackend {
		// consume the body to prevent goroutine leaks
		if ctx.request.Body != nil {
			if _, err := io.Copy(io.Discard, ctx.request.Body); err != nil {
				p.log.Errorf("error while discarding remainder request body: %v.", err)
			}
		}
		ctx.ensureDefaultResponse()
	} else if ctx.route.BackendType == eskip.LoopBackend {
		loopCTX := ctx.clone()
		if err := p.do(loopCTX); err != nil {
			return err
		}

		ctx.setResponse(loopCTX.response, p.flags.PreserveOriginal())
		ctx.proxySpan = loopCTX.proxySpan
	} else if p.flags.Debug() {
		debugReq, _, err := mapRequest(ctx, ctx.request.Context(), p.flags.HopHeadersRemoval())
		if err != nil {
			return &proxyError{err: err}
		}

		ctx.outgoingDebugRequest = debugReq
		ctx.setResponse(&http.Response{Header: make(http.Header)}, p.flags.PreserveOriginal())
	} else {

		done, allow := p.checkBreaker(ctx)
		if !allow {
			tracing.LogKV("circuit_breaker", "open", ctx.request.Context())
			return errCircuitBreakerOpen
		}

		backendContext := ctx.request.Context()
		if timeout, ok := ctx.StateBag()[filters.BackendTimeout]; ok {
			backendContext, ctx.cancelBackendContext = stdlibcontext.WithTimeout(backendContext, timeout.(time.Duration))
		}

		backendStart := time.Now()
		rsp, perr := p.makeBackendRequest(ctx, backendContext)
		if perr != nil {
			if done != nil {
				done(false)
			}

			p.metrics.IncErrorsBackend(ctx.route.Id)

			if retryable(ctx, perr) {
				if ctx.proxySpan != nil {
					ctx.proxySpan.Finish()
					ctx.proxySpan = nil
				}

				tracing.LogKV("retry", ctx.route.Id, ctx.Request().Context())

				perr = nil
				var perr2 *proxyError
				rsp, perr2 = p.makeBackendRequest(ctx, backendContext)
				if perr2 != nil {
					p.log.Errorf("Failed to retry backend request: %v", perr2)
					if perr2.code >= http.StatusInternalServerError {
						p.metrics.MeasureBackend5xx(backendStart)
					}
					return perr2
				}
			} else {
				return perr
			}
		}

		if rsp.StatusCode >= http.StatusInternalServerError {
			p.metrics.MeasureBackend5xx(backendStart)
		}

		if done != nil {
			done(rsp.StatusCode < http.StatusInternalServerError)
		}

		ctx.setResponse(rsp, p.flags.PreserveOriginal())
		p.metrics.MeasureBackend(ctx.route.Id, backendStart)
		p.metrics.MeasureBackendHost(ctx.route.Host, backendStart)
	}

	addBranding(ctx.response.Header)
	p.applyFiltersToResponse(processedFilters, ctx)
	return nil
}

func retryable(ctx *context, perr *proxyError) bool {
	req := ctx.Request()
	return perr.code != 499 && perr.DialError() &&
		ctx.route.BackendType == eskip.LBBackend &&
		req != nil && (req.Body == nil || req.Body == http.NoBody)
}

func (p *Proxy) serveResponse(ctx *context) {
	if p.flags.Debug() {
		dbgResponse(ctx.responseWriter, &debugInfo{
			route:        &ctx.route.Route,
			incoming:     ctx.originalRequest,
			outgoing:     ctx.outgoingDebugRequest,
			response:     ctx.response,
			filterPanics: ctx.debugFilterPanics,
		})

		return
	}

	start := time.Now()
	p.tracing.logStreamEvent(ctx.proxySpan, StreamHeadersEvent, StartEvent)
	copyHeader(ctx.responseWriter.Header(), ctx.response.Header)

	if err := ctx.Request().Context().Err(); err != nil {
		// deadline exceeded or canceled in stdlib, client closed request
		// see https://github.com/zalando/skipper/pull/864
		p.log.Debugf("Client request: %v", err)
		ctx.response.StatusCode = 499
		p.tracing.setTag(ctx.proxySpan, ClientRequestStateTag, ClientRequestCanceled)
	}

	p.tracing.setTag(ctx.initialSpan, HTTPStatusCodeTag, uint16(ctx.response.StatusCode))

	ctx.responseWriter.WriteHeader(ctx.response.StatusCode)
	ctx.responseWriter.Flush()
	p.tracing.logStreamEvent(ctx.proxySpan, StreamHeadersEvent, EndEvent)

	n, err := copyStream(ctx.responseWriter, ctx.response.Body)
	p.tracing.logStreamEvent(ctx.proxySpan, StreamBodyEvent, strconv.FormatInt(n, 10))
	if err != nil {
		p.metrics.IncErrorsStreaming(ctx.route.Id)
		p.log.Debugf("error while copying the response stream: %v", err)
		p.tracing.setTag(ctx.proxySpan, ErrorTag, true)
		p.tracing.setTag(ctx.proxySpan, StreamBodyEvent, StreamBodyError)
		p.tracing.logStreamEvent(ctx.proxySpan, StreamBodyEvent, fmt.Sprintf("Failed to stream response: %v", err))
	} else {
		p.metrics.MeasureResponse(ctx.response.StatusCode, ctx.request.Method, ctx.route.Id, start)
	}
	p.metrics.MeasureServe(ctx.route.Id, ctx.metricsHost(), ctx.request.Method, ctx.response.StatusCode, ctx.startServe)
}

func (p *Proxy) errorResponse(ctx *context, err error) {
	perr, ok := err.(*proxyError)
	if ok && perr.handled {
		return
	}

	flowIdLog := ""
	flowId := ctx.Request().Header.Get(flowidFilter.HeaderName)
	if flowId != "" {
		flowIdLog = fmt.Sprintf(", flow id %s", flowId)
	}
	id := unknownRouteID
	backendType := unknownRouteBackendType
	backend := unknownRouteBackend
	if ctx.route != nil {
		id = ctx.route.Id
		backendType = ctx.route.BackendType.String()
		backend = fmt.Sprintf("%s://%s", ctx.request.URL.Scheme, ctx.request.URL.Host)
	}

	code := http.StatusInternalServerError
	switch {
	case err == errRouteLookupFailed:
		code = p.defaultHTTPStatus
		ctx.initialSpan.LogKV("event", "error", "message", errRouteLookup.Error())
	case ok && perr.code == -1:
		// -1 == dial connection refused
		code = http.StatusBadGateway
	case ok && perr.code != 0:
		code = perr.code
	}

	p.tracing.setTag(ctx.initialSpan, ErrorTag, true)
	p.tracing.setTag(ctx.initialSpan, HTTPStatusCodeTag, uint16(code))

	if p.flags.Debug() {
		di := &debugInfo{
			incoming:     ctx.originalRequest,
			outgoing:     ctx.outgoingDebugRequest,
			response:     ctx.response,
			err:          err,
			filterPanics: ctx.debugFilterPanics,
		}

		if ctx.route != nil {
			di.route = &ctx.route.Route
		}

		dbgResponse(ctx.responseWriter, di)
		return
	}

	if ok && len(perr.additionalHeader) > 0 {
		copyHeader(ctx.responseWriter.Header(), perr.additionalHeader)
	}

	msgPrefix := "error while proxying"
	logFunc := p.log.Errorf
	if code == 499 {
		msgPrefix = "client canceled"
		logFunc = p.log.Infof
	}
	req := ctx.Request()
	remoteAddr := remoteHost(req)
	uri := req.RequestURI
	if i := strings.IndexRune(uri, '?'); i >= 0 {
		uri = uri[:i]
	}
	logFunc(
		`%s after %v, route %s with backend %s %s%s, status code %d: %v, remote host: %s, request: "%s %s %s", user agent: "%s"`,
		msgPrefix,
		time.Since(ctx.startServe),
		id,
		backendType,
		backend,
		flowIdLog,
		code,
		err,
		remoteAddr,
		req.Method,
		uri,
		req.Proto,
		req.UserAgent(),
	)

	p.sendError(ctx, id, code)
}

// strip port from addresses with hostname, ipv4 or ipv6
func stripPort(address string) string {
	if h, _, err := net.SplitHostPort(address); err == nil {
		return h
	}

	return address
}

// The remote address of the client. When the 'X-Forwarded-For'
// header is set, then it is used instead.
func remoteAddr(r *http.Request) string {
	ff := r.Header.Get("X-Forwarded-For")
	if ff != "" {
		return ff
	}

	return r.RemoteAddr
}

func remoteHost(r *http.Request) string {
	a := remoteAddr(r)
	return stripPort(a)
}

func shouldLog(statusCode int, filter *al.AccessLogFilter) bool {
	if len(filter.Prefixes) == 0 {
		return filter.Enable
	}
	match := false
	for _, prefix := range filter.Prefixes {
		switch {
		case prefix < 10:
			match = (statusCode >= prefix*100 && statusCode < (prefix+1)*100)
		case prefix < 100:
			match = (statusCode >= prefix*10 && statusCode < (prefix+1)*10)
		default:
			match = statusCode == prefix
		}
		if match {
			break
		}
	}
	return match == filter.Enable
}

// http.Handler implementation
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lw := logging.NewLoggingWriter(w)

	p.metrics.IncCounter("incoming." + r.Proto)
	var ctx *context

	var span ot.Span
	if wireContext, err := p.tracing.tracer.Extract(ot.HTTPHeaders, ot.HTTPHeadersCarrier(r.Header)); err != nil {
		span = p.tracing.tracer.StartSpan(p.tracing.initialOperationName)
	} else {
		span = p.tracing.tracer.StartSpan(p.tracing.initialOperationName, ext.RPCServerOption(wireContext))
	}
	defer func() {
		if ctx != nil && ctx.proxySpan != nil {
			ctx.proxySpan.Finish()
		}
		span.Finish()
	}()

	defer func() {
		accessLogEnabled, ok := ctx.stateBag[al.AccessLogEnabledKey].(*al.AccessLogFilter)

		if !ok {
			if p.accessLogDisabled {
				accessLogEnabled = &disabledAccessLog
			} else {
				accessLogEnabled = &enabledAccessLog
			}
		}
		statusCode := lw.GetCode()

		if shouldLog(statusCode, accessLogEnabled) {
			entry := &logging.AccessEntry{
				Request:      r,
				ResponseSize: lw.GetBytes(),
				StatusCode:   statusCode,
				RequestTime:  ctx.startServe,
				Duration:     time.Since(ctx.startServe),
			}

			additionalData, _ := ctx.stateBag[al.AccessLogAdditionalDataKey].(map[string]interface{})

			logging.LogAccess(entry, additionalData)
		}

		// This flush is required in I/O error
		if !ctx.successfulUpgrade {
			lw.Flush()
		}
	}()

	if p.flags.patchPath() {
		r.URL.Path = rfc.PatchPath(r.URL.Path, r.URL.RawPath)
	}

	p.tracing.
		setTag(span, SpanKindTag, SpanKindServer).
		setTag(span, HTTPRemoteIPTag, stripPort(r.RemoteAddr))
	p.setCommonSpanInfo(r.URL, r, span)
	r = r.WithContext(ot.ContextWithSpan(r.Context(), span))

	ctx = newContext(lw, r, p)
	ctx.startServe = time.Now()
	ctx.tracer = p.tracing.tracer
	ctx.initialSpan = span

	defer func() {
		if ctx.response != nil && ctx.response.Body != nil {
			err := ctx.response.Body.Close()
			if err != nil {
				p.log.Errorf("error during closing the response body: %v", err)
			}
		}
	}()

	err := p.do(ctx)

	if err != nil {
		p.errorResponse(ctx, err)
	} else {
		p.serveResponse(ctx)
	}

	if ctx.cancelBackendContext != nil {
		ctx.cancelBackendContext()
	}
}

// Close causes the proxy to stop closing idle
// connections and, currently, has no other effect.
// It's primary purpose is to support testing.
func (p *Proxy) Close() error {
	close(p.quit)
	return nil
}

func (p *Proxy) setCommonSpanInfo(u *url.URL, r *http.Request, s ot.Span) {
	p.tracing.
		setTag(s, ComponentTag, "skipper").
		setTag(s, HTTPMethodTag, r.Method).
		setTag(s, HostnameTag, p.hostname).
		setTag(s, HTTPPathTag, u.Path).
		setTag(s, HTTPHostTag, r.Host)
	if val := r.Header.Get("X-Flow-Id"); val != "" {
		p.tracing.setTag(s, FlowIDTag, val)
	}
}

// TODO(sszuecs): copy from net.Client, we should refactor this to use net.Client
func injectClientTrace(req *http.Request, span ot.Span) *http.Request {
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			span.LogKV("DNS", "start")
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			span.LogKV("DNS", "end")
		},
		ConnectStart: func(string, string) {
			span.LogKV("connect", "start")
		},
		ConnectDone: func(string, string, error) {
			span.LogKV("connect", "end")
		},
		TLSHandshakeStart: func() {
			span.LogKV("TLS", "start")
		},
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			span.LogKV("TLS", "end")
		},
		GetConn: func(string) {
			span.LogKV("get_conn", "start")
		},
		GotConn: func(httptrace.GotConnInfo) {
			span.LogKV("get_conn", "end")
		},
		WroteHeaders: func() {
			span.LogKV("wrote_headers", "done")
		},
		WroteRequest: func(wri httptrace.WroteRequestInfo) {
			if wri.Err != nil {
				span.LogKV("wrote_request", wri.Err.Error())
			} else {
				span.LogKV("wrote_request", "done")
			}
		},
		GotFirstResponseByte: func() {
			span.LogKV("got_first_byte", "done")
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
}
