// Package client sends snapshots to the Ping Pilott API and says, in terms the agent can act on, what happened.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Jatin-1111/pingpilot-agent/internal/config"
	"github.com/Jatin-1111/pingpilot-agent/internal/snapshot"
)

// MaxBody is how big one request may be. The API refuses more than 10 KB, so a batch is split before it gets there.
const MaxBody = 8 * 1024

// Kind is what the agent does next: each kind is handled differently by the run loop.
type Kind int

const (
	Unauthorized Kind = iota // 401: the token is not (or no longer) valid
	Forbidden                // 403: the token is fine, the account's plan does not include host metrics
	Rejected                 // 400, 413, 422: the API did not like what was sent; sending it again will not help
	RateLimited              // 429: too fast, try again after a while
	Unavailable              // the network, a 5xx, a 404 (the feature is switched off): try again later
)

// Error is a failed request, with enough to decide what to do and what to tell the person.
type Error struct {
	Kind       Kind
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
	Issues     []string
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "request failed"
	}
	if e.Status != 0 {
		return fmt.Sprintf("%s (HTTP %d)", msg, e.Status)
	}
	return msg
}

// Response is what a successful request brings back.
type Response struct {
	Accepted        int
	NextIntervalSec int
	MinAgentVersion string
}

type Client struct {
	API       string
	Token     string
	UserAgent string
	HTTP      *http.Client
	// TokenFunc, when set, gives the token for each request, so a token replaced in its file is picked up without a restart.
	TokenFunc func() string
}

// token is the token to use now: the freshly read one, or the one the agent started with when that cannot be read (a file
// being rewritten, for a moment).
func (c *Client) token() string {
	if c.TokenFunc != nil {
		if t := c.TokenFunc(); t != "" {
			return t
		}
	}
	return c.Token
}

func New(api, token, userAgent string) *Client {
	return &Client{
		API: api, Token: token, UserAgent: userAgent,
		HTTP: &http.Client{
			Timeout: 20 * time.Second,
			// The token goes in a header. A redirect is never followed, so it cannot be handed to another address.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

type apiBody struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Code    string `json:"code"`
	Data    struct {
		Accepted        int    `json:"accepted"`
		NextIntervalSec int    `json:"nextIntervalSec"`
		MinAgentVersion string `json:"minAgentVersion"`
	} `json:"data"`
	Issues []struct {
		Path    string `json:"path"`
		Message string `json:"message"`
	} `json:"issues"`
}

// Send posts one payload.
func (c *Client) Send(ctx context.Context, p snapshot.Payload) (Response, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return Response{}, &Error{Kind: Rejected, Message: "could not encode the snapshot: " + err.Error()}
	}
	return c.post(ctx, body)
}

func (c *Client) post(ctx context.Context, body []byte) (Response, error) {
	token := c.token()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.API+"/api/agent/v1/metrics", bytes.NewReader(body))
	if err != nil {
		return Response{}, &Error{Kind: Unavailable, Message: "bad API address: " + config.RedactIn(err.Error(), token)}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)

	res, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, &Error{Kind: Unavailable, Message: "could not reach the API: " + config.RedactIn(simplify(err), token)}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64*1024))

	var parsed apiBody
	_ = json.Unmarshal(raw, &parsed) // an error page that is not JSON still has a status

	if res.StatusCode == http.StatusOK {
		return Response{Accepted: parsed.Data.Accepted, NextIntervalSec: parsed.Data.NextIntervalSec, MinAgentVersion: parsed.Data.MinAgentVersion}, nil
	}

	e := &Error{Status: res.StatusCode, Code: parsed.Code, Message: config.RedactIn(parsed.Message, token)}
	for _, i := range parsed.Issues {
		e.Issues = append(e.Issues, strings.TrimSpace(i.Path+": "+i.Message))
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		e.Kind = Unauthorized
	case res.StatusCode == http.StatusForbidden:
		e.Kind = Forbidden
	case res.StatusCode == http.StatusTooManyRequests:
		e.Kind = RateLimited
		e.RetryAfter = retryAfter(res.Header.Get("Retry-After"))
	case res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusRequestEntityTooLarge || res.StatusCode == http.StatusUnprocessableEntity:
		e.Kind = Rejected
	case res.StatusCode >= 300 && res.StatusCode < 400:
		e.Kind = Unavailable
		e.Message = "the API address redirects somewhere else (check it is the https address)"
	default:
		e.Kind = Unavailable
		if res.StatusCode == http.StatusNotFound {
			e.Message = "this Ping Pilott server is not accepting host metrics (switched off, or the address is wrong)"
		}
	}
	if e.Message == "" {
		e.Message = http.StatusText(res.StatusCode)
	}
	return Response{}, e
}

// SendAll sends the snapshots in as many requests as the size limit needs, oldest first, and stops at the first failure.
// accepted is how many snapshots the API took, so the caller keeps only the rest.
func (c *Client) SendAll(ctx context.Context, host snapshot.Host, snaps []snapshot.Snapshot) (accepted int, last Response, err error) {
	for len(snaps) > 0 {
		n, body, encErr := fit(host, snaps)
		if encErr != nil {
			return accepted, last, &Error{Kind: Rejected, Message: "could not encode the snapshot: " + encErr.Error()}
		}
		resp, sendErr := c.post(ctx, body)
		if sendErr != nil {
			return accepted, last, sendErr
		}
		accepted += n
		last = resp
		snaps = snaps[n:]
	}
	return accepted, last, nil
}

// fit takes as many snapshots from the front as fit in MaxBody (at least one) and returns how many, and the request body.
func fit(host snapshot.Host, snaps []snapshot.Snapshot) (int, []byte, error) {
	var best []byte
	n := 0
	for n < len(snaps) && n < 10 {
		body, err := json.Marshal(snapshot.Payload{V: 1, Host: host, Snapshots: snaps[:n+1]})
		if err != nil {
			return 0, nil, err
		}
		if len(body) > MaxBody && n > 0 {
			break
		}
		best, n = body, n+1
	}
	return n, best, nil
}

func retryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		if d := time.Until(at); d > 0 {
			return d
		}
	}
	return 0
}

// simplify trims a network error to the part a person can use ("connection refused", "no such host").
func simplify(err error) string {
	var text = err.Error()
	// url.Error reads: Post "https://host/path": dial tcp ...: reason. The address is known to the person; the reason is not.
	if i := strings.LastIndex(text, ": "); i >= 0 && i+2 < len(text) {
		text = text[i+2:]
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	return text
}
