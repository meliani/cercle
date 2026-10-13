package ingest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Outcome is how the caller should treat a delivery attempt.
type Outcome int

const (
	// Delivered: the hub accepted the event (2xx).
	Delivered Outcome = iota
	// RetryLater: transient failure (network error, timeout, 408, 429, 5xx).
	RetryLater
	// Unrecoverable: the hub rejected the event (other 3xx/4xx); retrying will not help.
	Unrecoverable
)

func (o Outcome) String() string {
	switch o {
	case Delivered:
		return "delivered"
	case RetryLater:
		return "retry_later"
	default:
		return "unrecoverable"
	}
}

// Result of one delivery attempt.
type Result struct {
	Outcome    Outcome
	Status     int    // HTTP status, 0 when the request never got a response
	RetryAfter int    // seconds, from a Retry-After header; 0 when absent
	Detail     string // human-readable reason for logs (never contains the secret)
}

// Classify maps an HTTP round trip to an Outcome. err is a transport error
// (no response); status/headers come from the response otherwise.
func Classify(status int, headers map[string]string, err error) Result {
	if err != nil {
		return Result{Outcome: RetryLater, Detail: "hub unreachable: " + err.Error()}
	}
	r := Result{Status: status}
	switch {
	case status >= 200 && status < 300:
		r.Outcome = Delivered
	case status == 408 || status == 429 || status >= 500:
		r.Outcome = RetryLater
		r.RetryAfter = retryAfter(headers)
		r.Detail = fmt.Sprintf("hub returned HTTP %d", status)
	default:
		r.Outcome = Unrecoverable
		r.Detail = fmt.Sprintf("hub rejected event with HTTP %d", status)
		if status == 401 {
			r.Detail += " (check ingest_secret)"
		}
	}
	return r
}

func retryAfter(headers map[string]string) int {
	for k, v := range headers {
		if strings.EqualFold(k, "Retry-After") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// Request is everything needed for one POST to the hub.
type Request struct {
	URL     string
	Headers map[string]string
	Body    []byte
}

// UserAgent sent with every ingest request.
const UserAgent = "cercle-navidrome-plugin"

// BuildRequest marshals the event and sets the auth headers.
func BuildRequest(hubURL, secret string, ev Event) (Request, error) {
	body, err := json.Marshal(ev)
	if err != nil {
		return Request{}, err
	}
	return Request{
		URL: EndpointURL(hubURL),
		Headers: map[string]string{
			"Authorization": "Bearer " + secret,
			"Content-Type":  "application/json",
			"User-Agent":    UserAgent,
		},
		Body: body,
	}, nil
}

// Poster performs the HTTP POST. In the plugin it wraps host.HTTPSend.
type Poster func(Request) (status int, headers map[string]string, err error)

// Deliver sends one event and classifies the result.
func Deliver(post Poster, hubURL, secret string, ev Event) Result {
	req, err := BuildRequest(hubURL, secret, ev)
	if err != nil {
		return Result{Outcome: Unrecoverable, Detail: "encode event: " + err.Error()}
	}
	status, headers, err := post(req)
	return Classify(status, headers, err)
}

// ScrobbleErrorToken is the string Navidrome's scrobbler adapter matches on
// for a Scrobble result: "scrobbler(retry_later)", "scrobbler(retry_later:N)"
// or "scrobbler(unrecoverable)". Empty for Delivered.
func ScrobbleErrorToken(r Result) string {
	switch r.Outcome {
	case Delivered:
		return ""
	case RetryLater:
		if r.RetryAfter > 0 {
			return fmt.Sprintf("scrobbler(retry_later:%d)", r.RetryAfter)
		}
		return "scrobbler(retry_later)"
	default:
		return "scrobbler(unrecoverable)"
	}
}
