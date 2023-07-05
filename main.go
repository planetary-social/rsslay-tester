package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/boreq/errors"
	"github.com/gorilla/websocket"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/sirupsen/logrus"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

var addr = flag.String("addr", "wss://rss.nos.social", "relay address starting with ws:// or wss://")
var author = flag.String("author", "", "author hex or npub")

const (
	KindNote                = 1
	KindLongFormTextContent = 30023
)

func main() {
	logger := logrus.New()
	logger.SetLevel(logrus.DebugLevel)

	t := NewTester(logger)
	t.Run()
}

type Tester struct {
	eventsReceivedFromSubscriptions     map[string]*Subscription
	eventsReceivedFromSubscriptionsLock sync.Mutex

	subscriptionId int

	logger logrus.FieldLogger
}

func NewTester(logger logrus.FieldLogger) *Tester {
	return &Tester{
		eventsReceivedFromSubscriptions: make(map[string]*Subscription),
		logger:                          logger,
	}
}

func (t *Tester) Run() {
	flag.Parse()
	log.SetFlags(0)
	addr := *addr
	author := *author

	if author == "" {
		t.logger.Error("please provide a feed id from this rsslay instance")
		return
	}

	if strings.HasPrefix(author, "npub") {
		_, value, err := nip19.Decode(author)
		if err != nil {
			t.logger.WithError(err).Error("error parsing author npub")
			return
		}
		author = value.(string)
	}

	t.logger = t.logger.WithField("address", addr)
	t.logger.Debug("Connecting to the relay")

	c, _, err := websocket.DefaultDialer.Dial(addr, nil)
	if err != nil {
		t.logger.WithError(err).Error("Error connecting to the relay")
		return
	}
	defer c.Close()

	t.logger.Info("✔ Connected to the relay")

	go func() {
		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				t.logger.WithError(err).Error("read error")
				return
			}

			envelope := nostr.ParseMessage(message)
			if envelope == nil {
				t.logger.WithField("message", string(message)).Error("unknown message")
				continue
			}

			switch v := envelope.(type) {
			case *nostr.EventEnvelope:
				t.eventsReceivedFromSubscriptionsLock.Lock()
				t.eventsReceivedFromSubscriptions[*v.SubscriptionID].AddEvent(v.Event)
				t.eventsReceivedFromSubscriptionsLock.Unlock()
			case *nostr.EOSEEnvelope:
				t.eventsReceivedFromSubscriptionsLock.Lock()
				t.eventsReceivedFromSubscriptions[string(*v)].ReceivedEOSE()
				t.eventsReceivedFromSubscriptionsLock.Unlock()
			default:
				t.logger.WithField("message", envelope).Debug("received a message")
			}
		}
	}()

	for _, hexPublicKey := range []string{author} {
		if err := t.grabStoredEvents(c, nostr.Filter{Authors: []string{hexPublicKey}}); err != nil {
			t.logger.WithError(err).Error("Error getting events from the relay")
			return
		}

		if err := t.grabStoredEvents(c, nostr.Filter{Authors: []string{hexPublicKey}, Kinds: []int{KindNote}}); err != nil {
			t.logger.WithError(err).Error("Error getting events from the relay")
			return
		}

		if err := t.grabStoredEvents(c, nostr.Filter{Authors: []string{hexPublicKey}, Kinds: []int{KindLongFormTextContent}}); err != nil {
			t.logger.WithError(err).Error("Error getting events from the relay")
			return
		}

		if err := t.grabStoredEvents(c, nostr.Filter{Authors: []string{hexPublicKey}, Kinds: []int{KindNote, KindLongFormTextContent}}); err != nil {
			t.logger.WithError(err).Error("Error getting events from the relay")
			return
		}
	}

	waitForIters := 5
	iter := 0
	for {
		if ok := t.printEvents(); ok {
			break
		}

		<-time.After(1 * time.Second)

		if iter > waitForIters {
			t.logger.Error("opened subscriptions didn't receive EOSE on time")
			return
		}

		iter++
	}
}

func (t *Tester) printEvents() bool {
	t.eventsReceivedFromSubscriptionsLock.Lock()
	defer t.eventsReceivedFromSubscriptionsLock.Unlock()

	for _, subscription := range t.eventsReceivedFromSubscriptions {
		if !subscription.EOSE {
			return false
		}
	}

	for subscriptionId, subscription := range t.eventsReceivedFromSubscriptions {
		subscriptionId := subscriptionId
		subscription := subscription

		logger := t.logger.
			WithField("subscriptionId", subscriptionId).
			WithField("filter", subscription.Filter.String())

		if len(subscription.Events) == 0 {
			logger.Warning("⚠️ ️Subscription received no events (this can be good or bad, please think about this result)")
		}

		eventKindToNumberOfEvents := make(map[int]int)
		for _, event := range subscription.Events {
			eventKindToNumberOfEvents[event.Kind]++
		}

		for kind, count := range eventKindToNumberOfEvents {
			logger.
				WithField("eventKind", kind).
				WithField("count", count).
				Info("✔ ️Subscription received stored events (this can be good or bad, please think about this result)")
		}
	}

	return true
}

func (t *Tester) grabStoredEvents(c *websocket.Conn, filter nostr.Filter) error {
	t.eventsReceivedFromSubscriptionsLock.Lock()
	defer t.eventsReceivedFromSubscriptionsLock.Unlock()

	t.subscriptionId++
	subId := fmt.Sprintf("subscription-%d", t.subscriptionId)

	filterJSON, err := json.Marshal(filter)
	if err != nil {
		return errors.Wrap(err, "error marshaling the filter")
	}

	request := fmt.Sprintf(`["REQ", "subscription-%d", %s]`, t.subscriptionId, string(filterJSON))

	err = c.WriteMessage(websocket.TextMessage, []byte(request))
	if err != nil {
		return errors.Wrap(err, "error writing to socket")
	}

	t.eventsReceivedFromSubscriptions[subId] = NewSubscription(filter)

	t.logger.WithField("request", request).Trace("sent request")

	return nil
}

type Subscription struct {
	EOSE   bool
	Events []nostr.Event
	Filter nostr.Filter
}

func NewSubscription(filter nostr.Filter) *Subscription {
	return &Subscription{
		Filter: filter,
	}
}

func (s *Subscription) AddEvent(event nostr.Event) {
	s.Events = append(s.Events, event)
}

func (s *Subscription) ReceivedEOSE() {
	s.EOSE = true
}
