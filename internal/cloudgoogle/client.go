// Package cloudgoogle transports occurrence signals; it never receives pack inputs.
package cloudgoogle

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var Subscription = regexp.MustCompile(`^projects/[a-z][a-z0-9-]{4,61}[a-z0-9]/subscriptions/[A-Za-z][A-Za-z0-9._~+%-]{2,254}$`)
var Job = regexp.MustCompile(`^projects/[a-z][a-z0-9-]{4,61}[a-z0-9]/locations/[a-z0-9-]+/jobs/[A-Za-z0-9_-]{1,500}$`)
var Topic = regexp.MustCompile(`^projects/[a-z][a-z0-9-]{4,61}[a-z0-9]/topics/[A-Za-z][A-Za-z0-9._~+%-]{2,254}$`)

const scope = "https://www.googleapis.com/auth/pubsub"

type Signal struct {
	Version     int    `json:"version"`
	Job         string `json:"job"`
	ScheduledAt string `json:"scheduledAt"`
}
type Delivery struct {
	AckID   string `json:"ackId"`
	Message struct {
		Data string `json:"data"`
		ID   string `json:"messageId"`
	} `json:"message"`
}
type Client struct {
	HTTP     *http.Client
	endpoint string
}

func rejectRedirect(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
func New(ctx context.Context, credentialsFile string) (*Client, error) {
	if !filepath.IsAbs(credentialsFile) {
		return nil, errors.New("choose an absolute ADC file")
	}
	f, e := os.Open(credentialsFile)
	if e != nil {
		return nil, errors.New("cloud credential file unavailable")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > 128<<10 {
		return nil, errors.New("invalid cloud credential file")
	}
	raw, e := io.ReadAll(io.LimitReader(f, 128<<10+1))
	if e != nil || len(raw) > 128<<10 {
		return nil, errors.New("invalid cloud credential file")
	}
	tokenHTTP := &http.Client{Timeout: 30 * time.Second, CheckRedirect: rejectRedirect}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, tokenHTTP)
	creds, e := google.CredentialsFromJSON(ctx, raw, scope)
	if e != nil {
		return nil, errors.New("invalid ADC configuration")
	}
	return withToken(ctx, creds.TokenSource), nil
}

// Default is only for the deployed relay's attached service identity.
func Default(ctx context.Context) (*Client, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 30 * time.Second, CheckRedirect: rejectRedirect})
	creds, e := google.FindDefaultCredentials(ctx, scope)
	if e != nil {
		return nil, errors.New("cloud service identity unavailable")
	}
	return withToken(ctx, creds.TokenSource), nil
}
func withToken(ctx context.Context, ts oauth2.TokenSource) *Client {
	h := oauth2.NewClient(ctx, ts)
	h.Timeout = 45 * time.Second
	h.CheckRedirect = rejectRedirect
	return &Client{HTTP: h, endpoint: "https://pubsub.googleapis.com/v1/"}
}
func (c *Client) call(ctx context.Context, resource, method string, body, result any) error {
	raw, e := json.Marshal(body)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+resource+":"+method, bytes.NewReader(raw))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	res, e := c.HTTP.Do(req)
	if e != nil {
		return errors.New("Google Pub/Sub connection failed")
	}
	defer res.Body.Close()
	raw, e = io.ReadAll(io.LimitReader(res.Body, 64<<10+1))
	if e != nil || len(raw) > 64<<10 {
		return errors.New("Google Pub/Sub response exceeded its limit")
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("Google Pub/Sub returned HTTP %d", res.StatusCode)
	}
	if result != nil && json.Unmarshal(raw, result) != nil {
		return errors.New("invalid Google Pub/Sub response")
	}
	return nil
}
func (c *Client) Pull(ctx context.Context, subscription string) ([]Delivery, error) {
	if !Subscription.MatchString(subscription) {
		return nil, errors.New("invalid pull subscription")
	}
	var r struct {
		Received []Delivery `json:"receivedMessages"`
	}
	e := c.call(ctx, subscription, "pull", map[string]int{"maxMessages": 10}, &r)
	if len(r.Received) > 10 {
		return nil, errors.New("too many Pub/Sub deliveries")
	}
	return r.Received, e
}
func (c *Client) Ack(ctx context.Context, subscription, ack string) error {
	if !Subscription.MatchString(subscription) || len(ack) == 0 || len(ack) > 4096 {
		return errors.New("invalid acknowledgement")
	}
	return c.call(ctx, subscription, "acknowledge", map[string][]string{"ackIds": {ack}}, nil)
}
func (c *Client) Publish(ctx context.Context, topic string, s Signal) error {
	if !Topic.MatchString(topic) {
		return errors.New("invalid topic")
	}
	raw, e := json.Marshal(s)
	if e != nil {
		return e
	}
	return c.call(ctx, topic, "publish", map[string]any{"messages": []any{map[string]string{"data": base64.StdEncoding.EncodeToString(raw)}}}, nil)
}
