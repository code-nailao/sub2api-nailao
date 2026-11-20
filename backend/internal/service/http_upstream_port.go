package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// HTTPUpstream 上游 HTTP 请求接口
// 用于向上游 API（Claude、OpenAI、Gemini 等）发送请求
type HTTPUpstream interface {
	// Do 执行 HTTP 请求（不启用 TLS 指纹）
	Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error)

	// DoWithTLS 执行带 TLS 指纹伪装的 HTTP 请求
	//
	// profile 参数:
	//   - nil: 不启用 TLS 指纹，行为与 Do 方法相同
	//   - non-nil: 使用指定的 Profile 进行 TLS 指纹伪装
	//
	// Profile 由调用方通过 TLSFingerprintProfileService 解析后传入，
	// 支持按账号绑定的数据库 profile 或内置默认 profile。
	DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error)
}

type httpUpstreamDeliveryKey struct{}

type httpUpstreamDelivery struct {
	observable   atomic.Bool
	possiblySent atomic.Bool
}

func traceHTTPUpstreamDelivery(req *http.Request) (*http.Request, *httpUpstreamDelivery) {
	delivery := &httpUpstreamDelivery{}
	ctx := context.WithValue(req.Context(), httpUpstreamDeliveryKey{}, delivery)
	trace := &httptrace.ClientTrace{
		GetConn:      func(string) { delivery.observable.Store(true) },
		GotConn:      func(httptrace.GotConnInfo) { delivery.possiblySent.Store(true) },
		WroteHeaders: func() { delivery.possiblySent.Store(true) },
		// A failed write may already have sent part of the request.
		WroteRequest: func(httptrace.WroteRequestInfo) { delivery.possiblySent.Store(true) },
	}
	return req.WithContext(httptrace.WithClientTrace(ctx, trace)), delivery
}

// MarkHTTPUpstreamDeliveryObservable is called before preflight by transports
// that pass the request trace to net/http on every send. It makes an absent
// connection/write event meaningful even when preflight itself fails.
func MarkHTTPUpstreamDeliveryObservable(req *http.Request) {
	if req == nil {
		return
	}
	if delivery, ok := req.Context().Value(httpUpstreamDeliveryKey{}).(*httpUpstreamDelivery); ok {
		delivery.observable.Store(true)
	}
}

func (d *httpUpstreamDelivery) definitelyUnsent(err error) bool {
	if d.possiblySent.Load() {
		return false
	}
	if d.observable.Load() {
		return true
	}
	// Untouched traces from other HTTPUpstream implementations tell us
	// nothing about delivery. Still accept typed DNS and dial failures.
	var dnsErr *net.DNSError
	var opErr *net.OpError
	return errors.As(err, &dnsErr) || (errors.As(err, &opErr) && opErr.Op == "dial")
}
