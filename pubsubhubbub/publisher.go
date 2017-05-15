package pubsubhubbub

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/emersion/go-ostatus/activitystream"

	"log"
)

// DefaultLease is the default duration of a lease, if none is provided by the
// subscriber.
var DefaultLease = 24 * time.Hour

func writeEvent(w io.Writer, feed *activitystream.Feed) (mediaType string, err error) {
	return "application/atom+xml", feed.WriteTo(w)
}

// A Backend is used to build a publisher.
type Backend interface {
	// Subscribe sends content notifications about a topic to notifies in a new
	// goroutine. The notifies channel should only be closed after a call to
	// Unsubscribe. If the subscription is not possible, it should return a
	// DeniedError.
	Subscribe(topic string, notifies chan<- *activitystream.Feed) error
	// Unsubscribe closes notifies. The notifies channel must have been provided
	// to Subscribe.
	Unsubscribe(notifies chan<- *activitystream.Feed) error
}

type pubSubscription struct {
	notifies  chan *activitystream.Feed
	callbacks map[string]*pubCallback
}

type pubCallback struct {
	secret string
	timer  *time.Timer
}

func (s *pubSubscription) receive(c *http.Client) error {
	for notif := range s.notifies {
		var b bytes.Buffer
		mediaType, err := writeEvent(&b, notif)
		if err != nil {
			return err
		}

		// TODO: retry if a request fails
		for callbackURL, cb := range s.callbacks {
			body := bytes.NewReader(b.Bytes())
			req, err := http.NewRequest(http.MethodPost, callbackURL, body)
			if err != nil {
				log.Println("pubsubhubbub: failed create notification:", err)
				continue
			}

			req.Header.Set("Content-Type", mediaType)

			if cb.secret != "" {
				h := hmac.New(sha1.New, []byte(cb.secret))
				h.Write(b.Bytes())
				sig := hex.EncodeToString(h.Sum(nil))
				req.Header.Set("X-Hub-Signature", "sha1="+sig)
			}

			resp, err := c.Do(req)
			if err != nil {
				log.Println("pubsubhubbub: failed to push notification:", err)
				continue
			}
			resp.Body.Close()

			if resp.StatusCode/100 != 2 {
				log.Println("pubsubhubbub: failed to push notification:", resp.StatusCode, resp.Status)
				continue
			}
		}
	}

	return nil
}

// A Publisher distributes content notifications.
type Publisher struct {
	// SubscriptionState specifies an optional callback function that is called
	// when a subscription changes state. leaseEnd is zero if the subscription
	// ends.
	SubscriptionState func(topicURL, callbackURL, secret string, leaseEnd time.Time)

	be            Backend
	c             *http.Client
	subscriptions map[string]*pubSubscription
}

// NewPublisher creates a new publisher.
func NewPublisher(be Backend) *Publisher {
	return &Publisher{
		be:            be,
		c:             new(http.Client),
		subscriptions: make(map[string]*pubSubscription),
	}
}

func (p *Publisher) subscribeIfNotExist(topicURL string) (*pubSubscription, error) {
	s, ok := p.subscriptions[topicURL]
	if !ok {
		notifies := make(chan *activitystream.Feed)
		if err := p.be.Subscribe(topicURL, notifies); err != nil {
			return nil, err
		}

		s = &pubSubscription{
			notifies:  notifies,
			callbacks: make(map[string]*pubCallback),
		}
		go s.receive(p.c)

		p.subscriptions[topicURL] = s
	}

	return s, nil
}

// Register registers an existing subscription. It can be used to restore
// subscriptions when restarting the server.
func (p *Publisher) Register(topicURL, callbackURL, secret string, leaseEnd time.Time) error {
	s, err := p.subscribeIfNotExist(topicURL)
	if err != nil {
		return err
	}

	s.callbacks[callbackURL] = &pubCallback{
		secret: secret,
		timer: time.AfterFunc(time.Now().Sub(leaseEnd), func() {
			p.unregister(topicURL, callbackURL)
		}),
	}

	if p.SubscriptionState != nil {
		p.SubscriptionState(topicURL, callbackURL, secret, leaseEnd)
	}

	return nil
}

func (p *Publisher) unregister(topicURL, callbackURL string) error {
	s, ok := p.subscriptions[topicURL]
	if !ok {
		return nil
	}
	c, ok := s.callbacks[callbackURL]
	if !ok {
		return nil
	}

	if !c.timer.Stop() {
		<-c.timer.C
	}

	delete(s.callbacks, callbackURL)
	if len(s.callbacks) == 0 {
		if err := p.be.Unsubscribe(s.notifies); err != nil {
			return err
		}
		delete(p.subscriptions, topicURL)
	}

	if p.SubscriptionState != nil {
		p.SubscriptionState(topicURL, callbackURL, c.secret, time.Time{})
	}

	return nil
}

func (p *Publisher) verify(u *url.URL, q url.Values) error {
	challenge, err := generateChallenge()
	if err != nil {
		return err
	}
	q.Set("hub.challenge", challenge)

	u.RawQuery = q.Encode()
	subResp, err := p.c.Get(u.String())
	if err != nil {
		return err
	}
	defer subResp.Body.Close()

	if subResp.StatusCode/100 != 2 {
		return HTTPError(subResp.StatusCode)
	}

	buf := make([]byte, len(challenge))
	if _, err := io.ReadFull(subResp.Body, buf); err != nil {
		return err
	} else if !bytes.Equal(buf, []byte(challenge)) {
		return errors.New("pubsubhubbub: invalid challenge")
	}

	return nil
}

func (p *Publisher) denied(u *url.URL, q url.Values, deniedErr DeniedError) error {
	q.Set("hub.mode", "denied")
	q.Set("hub.reason", string(deniedErr))
	u.RawQuery = q.Encode()
	resp, err := p.c.Get(u.String())
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Subscribe processes a subscribe request.
func (p *Publisher) Subscribe(topicURL, callbackURL, secret string, lease time.Duration) error {
	u, err := url.Parse(callbackURL)
	if err != nil {
		return err
	}
	q := u.Query()

	// Subscribe if necessary
	s, err := p.subscribeIfNotExist(topicURL)
	if deniedErr, ok := err.(DeniedError); ok {
		// Send denied notification
		return p.denied(u, q, deniedErr)
	} else if err != nil {
		return err
	}

	// Verify
	q.Set("hub.mode", "subscribe")
	q.Set("hub.topic", topicURL)
	q.Set("hub.lease_seconds", strconv.Itoa(int(lease.Seconds())))
	if err := p.verify(u, q); err != nil {
		return err
	}

	s.callbacks[callbackURL] = &pubCallback{
		secret: secret,
		timer: time.AfterFunc(lease, func() {
			p.unregister(topicURL, callbackURL)
		}),
	}
	return nil
}

// Unsubscribe processes an unsubscribe request.
func (p *Publisher) Unsubscribe(topicURL, callbackURL string) error {
	u, err := url.Parse(callbackURL)
	if err != nil {
		return err
	}
	q := u.Query()

	s, ok := p.subscriptions[topicURL]
	if !ok {
		return nil
	} else if _, ok := s.callbacks[callbackURL]; !ok {
		return nil
	}

	// Verify
	q.Set("hub.mode", "unsubscribe")
	q.Set("hub.topic", topicURL)
	if err := p.verify(u, q); err != nil {
		return err
	}

	return p.unregister(topicURL, callbackURL)
}

// ServeHTTP implements http.Handler.
func (p *Publisher) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()

	if req.Method != http.MethodPost {
		http.Error(resp, "Unsupported method", http.StatusMethodNotAllowed)
		return
	}

	mode := req.FormValue("hub.mode")
	callbackURL := req.FormValue("hub.callback")
	topicURL := req.FormValue("hub.topic")
	secret := req.FormValue("hub.secret")
	// TODO: hub.lease_seconds

	if mode != "subscribe" && mode != "unsubscribe" {
		http.Error(resp, "Invalid mode", http.StatusBadRequest)
		return
	}
	if len(secret) > 200 {
		http.Error(resp, "Secret too long", http.StatusBadRequest)
		return
	}

	go func() {
		var err error
		switch mode {
		case "subscribe":
			err = p.Subscribe(topicURL, callbackURL, secret, DefaultLease)
		case "unsubscribe":
			err = p.Unsubscribe(topicURL, callbackURL)
		}
		if err != nil {
			log.Println("pubsubhubbub: cannot %v to %q with callback %q: %v", mode, topicURL, callbackURL, err)
		}
	}()

	resp.WriteHeader(http.StatusAccepted)
}
