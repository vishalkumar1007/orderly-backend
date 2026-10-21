package events

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Broker is the JetStream connection. One per process; nil-safe methods so
// a deployment without NATS_URL still runs (events wait in the outbox).
type Broker struct {
	nc  *nats.Conn
	js  jetstream.JetStream
	log *slog.Logger
}

// streams maps each context to its stream; subjects are <context>.>.
var streams = []string{ContextPlatform, ContextCommerce, ContextInventory}

// Connect dials NATS and makes sure the three streams exist. An empty url
// returns a disabled broker rather than an error.
func Connect(ctx context.Context, url string, log *slog.Logger) (*Broker, error) {
	if url == "" {
		log.Warn("NATS_URL is not set: events stay in the outbox and no consumer runs")
		return &Broker{log: log}, nil
	}
	nc, err := nats.Connect(url,
		nats.Name("orderly-backend"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	for _, name := range streams {
		if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
			Name:      strings.ToUpper(name),
			Subjects:  []string{name + ".>"},
			Retention: jetstream.LimitsPolicy,
			MaxAge:    30 * 24 * time.Hour,
			Storage:   jetstream.FileStorage,
			// The relay republishes after a crash; this window drops the twin.
			Duplicates: 10 * time.Minute,
		}); err != nil {
			nc.Close()
			return nil, fmt.Errorf("ensure stream %s: %w", name, err)
		}
	}
	return &Broker{nc: nc, js: js, log: log}, nil
}

// Enabled reports whether a broker is connected.
func (b *Broker) Enabled() bool { return b != nil && b.js != nil }

// Close drains the connection.
func (b *Broker) Close() {
	if b != nil && b.nc != nil {
		_ = b.nc.Drain()
	}
}

// Publish implements Publisher with the event id as the dedupe key.
func (b *Broker) Publish(ctx context.Context, subject, msgID string, payload []byte) error {
	if !b.Enabled() {
		return errDisabled
	}
	_, err := b.js.Publish(ctx, subject, payload, jetstream.WithMsgID(msgID))
	return err
}

var errDisabled = fmt.Errorf("events: broker disabled")
