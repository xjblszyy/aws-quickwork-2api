package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"quickwork-openai/internal/accounts"
)

var ErrEmptyResponse = errors.New("upstream returned no visible text or tool calls")

type UpstreamError struct {
	StatusCode int
	Code       string
	Err        error
}

func (err *UpstreamError) Error() string {
	if err.Err != nil {
		return err.Err.Error()
	}
	return "upstream request failed"
}

func (err *UpstreamError) Unwrap() error {
	return err.Err
}

type retryAction int

const (
	retryStop retryAction = iota
	retryTemporary
	retryRefresh
	retryNextAccount
	retryNewSession
)

func retryActionFor(err error) retryAction {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrUserMessageTooLarge) {
		return retryStop
	}
	if errors.Is(err, ErrEmptyResponse) {
		return retryTemporary
	}
	var upstreamErr *UpstreamError
	if errors.As(err, &upstreamErr) {
		if action, known := retryActionForCode(upstreamErr.Code); known {
			return action
		}
		switch {
		case upstreamErr.StatusCode == http.StatusUnauthorized:
			return retryRefresh
		case upstreamErr.StatusCode == http.StatusForbidden || upstreamErr.StatusCode == http.StatusPaymentRequired:
			return retryNextAccount
		case upstreamErr.StatusCode == http.StatusRequestTimeout || upstreamErr.StatusCode == http.StatusTooManyRequests:
			return retryTemporary
		case upstreamErr.StatusCode >= 500 && upstreamErr.StatusCode <= 599:
			return retryTemporary
		case upstreamErr.StatusCode >= 400 && upstreamErr.StatusCode <= 499:
			return retryStop
		}
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseAbnormalClosure, websocket.CloseGoingAway, websocket.CloseInternalServerErr,
			websocket.CloseServiceRestart, websocket.CloseTryAgainLater:
			return retryTemporary
		default:
			return retryStop
		}
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return retryStop
		}
		if networkErr.Timeout() || networkErr.Temporary() || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
			return retryTemporary
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.EPIPE) {
		return retryTemporary
	}
	return retryStop
}

func retryActionForCode(code string) (retryAction, bool) {
	normalized := strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(code)))
	normalized = strings.TrimSuffix(normalized, "exception")
	switch normalized {
	case "expiredtoken", "tokenexpired", "invalidtoken", "invalidaccesstoken", "accesstokenexpired", "idtokenexpired",
		"unauthorized", "unauthenticated", "notauthenticated", "authenticationfailed":
		return retryRefresh, true
	case "accessdenied", "forbidden", "permissiondenied", "insufficientcredits", "insufficientquota", "quotaexceeded",
		"resourceexhausted", "creditsexhausted", "subscriptionrequired", "paymentrequired":
		return retryNextAccount, true
	case "sessionnotfound", "threadnotfound", "conversationnotfound", "invalidsession", "sessionexpired",
		"invalidthread", "threadexpired", "sessiondoesnotexist":
		return retryNewSession, true
	case "validation", "validationerror", "invalidargument", "invalidrequest", "invalidrequesterror", "badrequest",
		"malformedrequest", "prompttoolarge", "messagetoolarge", "requesttoolarge", "contextlengthexceeded":
		return retryStop, true
	case "throttling", "throttled", "toomanyrequests", "ratelimitexceeded", "ratelimited", "serviceunavailable", "unavailable",
		"internalservererror", "internalerror", "timeout", "requesttimeout", "gatewaytimeout", "upstreamtimeout",
		"overloaded", "serverbusy", "temporarilyunavailable":
		return retryTemporary, true
	default:
		return retryStop, false
	}
}

func invalidChatRequest(err error) bool {
	if errors.Is(err, ErrUserMessageTooLarge) {
		return true
	}
	var upstreamErr *UpstreamError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	if action, known := retryActionForCode(upstreamErr.Code); known {
		return action == retryStop
	}
	switch upstreamErr.StatusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func chatErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, accounts.ErrNoAccounts), errors.Is(err, accounts.ErrNoAvailableAccounts):
		return http.StatusUnauthorized, "upstream_session_unavailable"
	case errors.Is(err, ErrUserMessageTooLarge):
		return http.StatusRequestEntityTooLarge, "context_length_exceeded"
	case invalidChatRequest(err):
		return http.StatusBadRequest, "invalid_request_error"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "upstream_timeout"
	default:
		return http.StatusBadGateway, "upstream_error"
	}
}

func waitForRetry(ctx context.Context, attempt int) error {
	delay := 200 * time.Millisecond
	for count := 1; count < attempt && delay < 2*time.Second; count++ {
		delay *= 2
	}
	if delay > 2*time.Second {
		delay = 2 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
