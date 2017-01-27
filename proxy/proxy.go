package proxy

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"code.cloudfoundry.org/gorouter/access_log"
	"code.cloudfoundry.org/gorouter/access_log/schema"
	router_http "code.cloudfoundry.org/gorouter/common/http"
	"code.cloudfoundry.org/gorouter/config"
	"code.cloudfoundry.org/gorouter/handlers"
	"code.cloudfoundry.org/gorouter/logger"
	"code.cloudfoundry.org/gorouter/metrics/reporter"
	"code.cloudfoundry.org/gorouter/proxy/handler"
	"code.cloudfoundry.org/gorouter/proxy/round_tripper"
	"code.cloudfoundry.org/gorouter/proxy/utils"
	"code.cloudfoundry.org/gorouter/route"
	"code.cloudfoundry.org/gorouter/routeservice"
	"github.com/cloudfoundry/dropsonde"
	"github.com/uber-go/zap"
	"github.com/urfave/negroni"
)

const (
	VcapCookieId    = "__VCAP_ID__"
	StickyCookieKey = "JSESSIONID"
)

type Proxy interface {
	ServeHTTP(responseWriter http.ResponseWriter, request *http.Request)
}

type proxyHandler struct {
	handlers *negroni.Negroni
}

func (p *proxyHandler) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	p.handlers.ServeHTTP(responseWriter, request)
}

type proxyWriterHandler struct{}

// ServeHTTP wraps the responseWriter in a ProxyResponseWriter
func (p *proxyWriterHandler) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request, next http.HandlerFunc) {
	proxyWriter := utils.NewProxyResponseWriter(responseWriter)
	next(proxyWriter, request)
}

type proxy struct {
	ip                       string
	traceKey                 string
	logger                   logger.Logger
	reporter                 reporter.ProxyReporter
	accessLogger             access_log.AccessLogger
	secureCookies            bool
	heartbeatOK              *int32
	routeServiceConfig       *routeservice.RouteServiceConfig
	extraHeadersToLog        *[]string
	healthCheckUserAgent     string
	forceForwardedProtoHttps bool
	defaultLoadBalance       string
}

func NewProxy(
	logger logger.Logger,
	accessLogger access_log.AccessLogger,
	c *config.Config,
	registry handlers.LookupRegistry,
	reporter reporter.ProxyReporter,
	routeServiceConfig *routeservice.RouteServiceConfig,
	tlsConfig *tls.Config,
	heartbeatOK *int32,
) Proxy {

	p := &proxy{
		accessLogger:             accessLogger,
		traceKey:                 c.TraceKey,
		ip:                       c.Ip,
		logger:                   logger,
		reporter:                 reporter,
		secureCookies:            c.SecureCookies,
		heartbeatOK:              heartbeatOK, // 1->true, 0->false
		routeServiceConfig:       routeServiceConfig,
		extraHeadersToLog:        &c.ExtraHeadersToLog,
		healthCheckUserAgent:     c.HealthCheckUserAgent,
		forceForwardedProtoHttps: c.ForceForwardedProtoHttps,
		defaultLoadBalance:       c.LoadBalance,
	}

	httpTransport := &http.Transport{
		Dial: func(network, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout(network, addr, 5*time.Second)
			if err != nil {
				return conn, err
			}
			if c.EndpointTimeout > 0 {
				err = conn.SetDeadline(time.Now().Add(c.EndpointTimeout))
			}
			return conn, err
		},
		DisableKeepAlives:   c.DisableKeepAlives,
		MaxIdleConns:        c.MaxIdleConns,
		MaxIdleConnsPerHost: c.MaxIdleConnsPerHost,
		DisableCompression:  true,
		TLSClientConfig:     tlsConfig,
	}

	rproxy := &ReverseProxy{
		Transport:     p.proxyRoundTripper(httpTransport),
		FlushInterval: 50 * time.Millisecond,
		Director:      p.setupProxyRequest,
	}

	n := negroni.New()
	n.Use(&proxyWriterHandler{})
	n.Use(handlers.NewAccessLog(accessLogger, &c.ExtraHeadersToLog))
	n.Use(handlers.NewHealthcheck(c.HealthCheckUserAgent, p.heartbeatOK, logger))
	n.Use(handlers.NewZipkin(c.Tracing.EnableZipkin, &c.ExtraHeadersToLog, logger))
	n.Use(handlers.NewProtocolCheck(logger))
	n.Use(handlers.NewLookup(registry, reporter, logger))
	n.Use(p)
	n.Use(rproxy)

	handlers := &proxyHandler{
		handlers: n,
	}

	return handlers
}

func hostWithoutPort(req *http.Request) string {
	host := req.Host

	// Remove :<port>
	pos := strings.Index(host, ":")
	if pos >= 0 {
		host = host[0:pos]
	}

	return host
}

func (p *proxy) proxyRoundTripper(transport http.RoundTripper) http.RoundTripper {
	return round_tripper.NewProxyRoundTripper(dropsonde.InstrumentedRoundTripper(transport), p.logger, nil, p.defaultLoadBalance)
}

func (p *proxy) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request, next http.HandlerFunc) {
	proxyWriter := responseWriter.(utils.ProxyResponseWriter)

	alr := request.Context().Value("AccessLogRecord")
	if alr == nil {
		p.logger.Error("AccessLogRecord not set on context", zap.Error(errors.New("failed-to-access-LogRecord")))
		http.Error(responseWriter, "AccessLogRecord not set on context", http.StatusInternalServerError)
		return
	}
	accessLog := alr.(*schema.AccessLogRecord)

	handler := handler.NewRequestHandler(request, proxyWriter, p.reporter, accessLog, p.logger)

	rp := request.Context().Value("RoutePool")
	if rp == nil {
		p.logger.Error("RoutePool not set on context", zap.Error(errors.New("failed-to-access-RoutePool")))
		http.Error(responseWriter, "RoutePool not set on context", http.StatusInternalServerError)
		return
	}
	routePool := rp.(*route.Pool)

	stickyEndpointId := getStickySession(request)
	iter := &wrappedIterator{
		nested: routePool.Endpoints(p.defaultLoadBalance, stickyEndpointId),

		afterNext: func(endpoint *route.Endpoint) {
			if endpoint != nil {
				accessLog.RouteEndpoint = endpoint
				p.reporter.CaptureRoutingRequest(endpoint)
			}
		},
	}

	if isTcpUpgrade(request) {
		handler.HandleTcpRequest(iter)
		return
	}

	if isWebSocketUpgrade(request) {
		handler.HandleWebSocketRequest(iter)
		return
	}

	next(responseWriter, request)
}

func (p *proxy) setupProxyRequest(source *http.Request, target *http.Request) {
	if p.forceForwardedProtoHttps {
		target.Header.Set("X-Forwarded-Proto", "https")
	} else if source.Header.Get("X-Forwarded-Proto") == "" {
		scheme := "http"
		if source.TLS != nil {
			scheme = "https"
		}
		target.Header.Set("X-Forwarded-Proto", scheme)
	}

	target.URL.Scheme = "http"
	target.URL.Host = source.Host
	target.URL.Opaque = source.RequestURI
	target.URL.RawQuery = ""

	handler.SetRequestXRequestStart(source)
	target.Header.Del(router_http.CfAppInstance)
}

type wrappedIterator struct {
	nested    route.EndpointIterator
	afterNext func(*route.Endpoint)
}

func (i *wrappedIterator) Next() *route.Endpoint {
	e := i.nested.Next()
	if i.afterNext != nil {
		i.afterNext(e)
	}
	return e
}

func (i *wrappedIterator) EndpointFailed() {
	i.nested.EndpointFailed()
}
func (i *wrappedIterator) PreRequest(e *route.Endpoint) {
	i.nested.PreRequest(e)
}
func (i *wrappedIterator) PostRequest(e *route.Endpoint) {
	i.nested.PostRequest(e)
}

func setupStickySession(responseWriter http.ResponseWriter, response *http.Response,
	endpoint *route.Endpoint,
	originalEndpointId string,
	secureCookies bool,
	path string) {
	secure := false
	maxAge := 0

	// did the endpoint change?
	sticky := originalEndpointId != "" && originalEndpointId != endpoint.PrivateInstanceId

	for _, v := range response.Cookies() {
		if v.Name == StickyCookieKey {
			sticky = true
			if v.MaxAge < 0 {
				maxAge = v.MaxAge
			}
			secure = v.Secure
			break
		}
	}

	if sticky {
		// right now secure attribute would as equal to the JSESSION ID cookie (if present),
		// but override if set to true in config
		if secureCookies {
			secure = true
		}

		cookie := &http.Cookie{
			Name:     VcapCookieId,
			Value:    endpoint.PrivateInstanceId,
			Path:     path,
			MaxAge:   maxAge,
			HttpOnly: true,
			Secure:   secure,
		}

		http.SetCookie(responseWriter, cookie)
	}
}

func getStickySession(request *http.Request) string {
	// Try choosing a backend using sticky session
	if _, err := request.Cookie(StickyCookieKey); err == nil {
		if sticky, err := request.Cookie(VcapCookieId); err == nil {
			return sticky.Value
		}
	}
	return ""
}

func isWebSocketUpgrade(request *http.Request) bool {
	// websocket should be case insensitive per RFC6455 4.2.1
	return strings.ToLower(upgradeHeader(request)) == "websocket"
}

func isTcpUpgrade(request *http.Request) bool {
	return upgradeHeader(request) == "tcp"
}

func upgradeHeader(request *http.Request) string {
	// handle multiple Connection field-values, either in a comma-separated string or multiple field-headers
	for _, v := range request.Header[http.CanonicalHeaderKey("Connection")] {
		// upgrade should be case insensitive per RFC6455 4.2.1
		if strings.Contains(strings.ToLower(v), "upgrade") {
			return request.Header.Get("Upgrade")
		}
	}

	return ""
}
