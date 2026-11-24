//go:build !nomercure

package mercurehub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/dunglas/mercure"
	"github.com/prometheus/client_golang/prometheus"
)

// Hub is the hub of the process: one HTTP server serves its clients, and
// mercure_publish() publishes to it from any PHP thread.
type Hub struct {
	hub     *mercure.Hub
	log     *slog.Logger
	stopped *atomic.Bool
}

var running atomic.Pointer[Hub]

// Start makes the declared hub. With reg, the hub's own metrics
// (mercure_*) are collected there.
func Start(d Declaration, log *slog.Logger, reg prometheus.Registerer) (*Hub, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	stopped := new(atomic.Bool)
	opts, err := d.options(slog.New(hubLog{mercure.NewSlogHandler(log.Handler()), stopped}))
	if err != nil {
		return nil, err
	}
	if reg != nil {
		opts = append(opts, mercure.WithMetrics(mercure.NewPrometheusMetrics(reg)))
	}
	hub, err := mercure.NewHub(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("mercure: %w", err)
	}
	h := &Hub{hub: hub, log: log, stopped: stopped}
	if !running.CompareAndSwap(nil, h) {
		h.close()
		return nil, errors.New("mercure: a process has one hub, and mercure() is declared on two servers")
	}
	return h, nil
}

func (d *Declaration) options(log *slog.Logger) ([]mercure.Option, error) {
	opts := []mercure.Option{
		mercure.WithLogger(log),
		mercure.WithHeartbeat(seconds(d.Heartbeat)),
		mercure.WithWriteTimeout(seconds(d.WriteTimeout)),
		mercure.WithDispatchTimeout(seconds(d.DispatchTimeout)),
		mercure.WithCORSOrigins(d.CORSOrigins),
		mercure.WithPublishOrigins(d.PublishOrigins),
		mercure.WithSubscriptionLimits(mercure.SubscriptionLimits{
			Total: d.MaxSubscriptions, PerToken: d.MaxSubscriptionsPerToken, PerClient: d.MaxSubscriptionsPerClient,
		}),
	}
	if len(d.Issuers) > 0 {
		issuers := make([]mercure.Issuer, len(d.Issuers))
		for i, iss := range d.Issuers {
			issuers[i] = mercure.Issuer{
				Identifier: iss.Identifier,
				Publisher:  verifier(iss.Publisher, iss.Algorithm),
				Subscriber: verifier(iss.Subscriber, iss.Algorithm),
			}
		}
		opts = append(opts, mercure.WithIssuers(issuers))
	}
	if d.Compatibility != 0 {
		if !compatible {
			return nil, errors.New("mercure: compatibility() needs a cairo built with the tags deprecated_topic and deprecated_claim")
		}
		opts = append(opts, mercure.WithProtocolVersionCompatibility(d.Compatibility))
	}
	if d.History != "" {
		subscribers := mercure.NewSubscriberList(mercure.DefaultSubscriberListCacheSize)
		kept, err := mercure.NewBoltTransport(subscribers, log, d.History, "", d.HistorySize, mercure.BoltDefaultCleanupFrequency)
		if err != nil {
			return nil, fmt.Errorf("mercure: history %s: %w", d.History, err)
		}
		opts = append(opts, mercure.WithTransport(kept))
	}
	return append(opts, d.switches()...), nil
}

// switches are the options that are there or not.
func (d *Declaration) switches() []mercure.Option {
	var opts []mercure.Option
	if d.Anonymous {
		opts = append(opts, mercure.WithAnonymous())
	}
	if d.Subscriptions {
		opts = append(opts, mercure.WithSubscriptions())
	}
	if d.Debugger {
		opts = append(opts, mercure.WithDebugger())
	}
	if len(d.PublicURLs) > 0 {
		opts = append(opts, mercure.WithPublicURLs(d.PublicURLs))
	}
	if d.Resource != "" {
		opts = append(opts, mercure.WithResourceIdentifier(d.Resource))
	}
	if d.CookieName != "" {
		opts = append(opts, mercure.WithCookieName(d.CookieName))
	}
	return opts
}

// verifier is nil for a role without a key: the hub then takes no token of
// that role from the issuer.
func verifier(key, algorithm string) mercure.Verifier {
	if key == "" {
		return nil
	}
	return mercure.Static{Key: []byte(key), Algorithm: algorithm}
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// Routes answers the requests for the hub and gives every other to next.
func (h *Hub) Routes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Serves(r.URL.Path) {
			h.hub.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Stop ends the hub's subscribers, which a server that shuts down would
// otherwise wait for, and refuses what is published from then on. A server
// without a hub has a nil one.
func (h *Hub) Stop() {
	if h == nil {
		return
	}
	running.CompareAndSwap(h, nil)
	h.close()
}

func (h *Hub) close() {
	if h.stopped.Swap(true) {
		return
	}
	if err := h.hub.Stop(context.Background()); err != nil {
		h.log.Warn("mercure: stopping the hub", "error", err)
	}
}

// hubLog is what the hub logs: its warnings and errors, until it is
// stopped. Below that the hub reports every subscriber that comes and goes,
// which a server that logs no request unless asked (accessLog()) does not
// want. And every subscriber that a stop disconnects fails to take itself
// off the closed hub, which the hub reports as an error each time: a deploy
// would log one per client.
type hubLog struct {
	slog.Handler
	stopped *atomic.Bool
}

func (l hubLog) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn && !l.stopped.Load() && l.Handler.Enabled(ctx, level)
}

func (l hubLog) WithAttrs(attrs []slog.Attr) slog.Handler {
	return hubLog{l.Handler.WithAttrs(attrs), l.stopped}
}

func (l hubLog) WithGroup(name string) slog.Handler {
	return hubLog{l.Handler.WithGroup(name), l.stopped}
}

// Update is what mercure_publish() publishes.
type Update struct {
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
	Private bool     `json:"private"`
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Retry   uint64   `json:"retry"`
}

// ErrNoHub is Publish in a process that declared no hub, or stopped it.
var ErrNoHub = errors.New("no Mercure hub runs in this process: declare one with $http->mercure()")

// Publish hands an update to the hub's subscribers and returns its id. It
// does not wait for them: the hub drops a subscriber that does not keep up.
func Publish(ctx context.Context, u Update) (string, error) {
	h := running.Load()
	if h == nil {
		return "", ErrNoHub
	}
	update := &mercure.Update{
		Topics:  u.Topics,
		Private: u.Private,
		Event:   mercure.Event{Data: u.Data, ID: u.ID, Type: u.Type, Retry: u.Retry},
	}
	if err := h.hub.Publish(ctx, update); err != nil {
		if errors.Is(err, mercure.ErrClosedTransport) {
			return "", ErrNoHub
		}
		return "", err
	}
	return update.ID, nil
}

// invalid are the errors of Publish that are the update's own fault.
var invalid = []error{
	mercure.ErrMissingTopic, mercure.ErrTooManyTopics, mercure.ErrInvalidTopic,
	mercure.ErrReservedTopic, mercure.ErrReservedWildcard,
	mercure.ErrInvalidEventID, mercure.ErrInvalidEventType, mercure.ErrReservedEventType,
	mercure.ErrInvalidData,
}

// Invalid reports whether an error of Publish says that the update is not
// one the protocol allows (as opposed to a hub that could not hand it out).
func Invalid(err error) bool {
	for _, target := range invalid {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
