package regular

import (
	"context"
	"fmt"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/services"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"
)

type keepAliveState int

func (k keepAliveState) String() string {
	switch k {
	case heartbeatStateAnnounce:
		return "announce"
	case heartbeatStateKeepAlive:
		return "keep-alive"
	default:
		return fmt.Sprintf("unknown state %v", int(k))
	}
}

const (
	// heartbeatStateAnnounce is set when full
	// state has to be announced back to the auth server
	heartbeatStateAnnounce = iota
	// heartbeatStateKeepAlive is set when
	// only sending keep alives is necessary
	heartbeatStateKeepAlive = iota
)

func newHeartbeat(cfg heartbeatConfig) (*heartbeat, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	ctx, cancel := context.WithCancel(cfg.ctx)
	h := &heartbeat{
		cancelCtx:       ctx,
		cancel:          cancel,
		heartbeatConfig: cfg,
		Entry: log.WithFields(log.Fields{
			trace.Component: teleport.Component(cfg.component, "beat"),
		}),
		keepAliveTicker: time.NewTicker(cfg.keepAlivePeriod),
		announceTicker:  time.NewTicker(cfg.announcePeriod),
		announceC:       make(chan struct{}, 1),
		sendC:           make(chan struct{}, 1),
	}
	h.Debugf("Starting heartbeat with keep alive period %v, full update period: %v", cfg.keepAlivePeriod, cfg.announcePeriod)
	go h.run()
	return h, nil
}

type getServerInfoFn func() (services.Server, error)

type heartbeatConfig struct {
	proxyMode       bool
	ctx             context.Context
	component       string
	accessPoint     auth.AccessPoint
	getServerInfo   getServerInfoFn
	keepAlivePeriod time.Duration
	announcePeriod  time.Duration
	clock           clockwork.Clock
}

func (cfg *heartbeatConfig) CheckAndSetDefaults() error {
	if cfg.ctx == nil {
		return trace.BadParameter("missing parameter ctx")
	}
	if cfg.accessPoint == nil {
		return trace.BadParameter("missing parameter accessPoint")
	}
	if cfg.component == "" {
		return trace.BadParameter("missing parameter component")
	}
	if cfg.keepAlivePeriod == 0 {
		return trace.BadParameter("missing parameter keepAlivePeriod")
	}
	if cfg.announcePeriod == 0 {
		return trace.BadParameter("missing parameter announcePeriod")
	}
	if cfg.getServerInfo == nil {
		return trace.BadParameter("missing parameter getServerInfo")
	}
	if cfg.clock == nil {
		cfg.clock = clockwork.NewRealClock()
	}
	return nil
}

// heartbeat keeps heartbeat state, it is implemented
// according to actor model - all interactions with it are to be done
// with signals
type heartbeat struct {
	heartbeatConfig
	cancelCtx context.Context
	cancel    context.CancelFunc
	*log.Entry
	state     keepAliveState
	current   services.Server
	keepAlive *services.KeepAlive
	// keepAliveTicker is a faster timer to send keep alives
	keepAliveTicker *time.Ticker
	// announceTicker is a slower timers doing full update
	announceTicker *time.Ticker
	// keepAliver sends keep alive updates
	keepAliver services.KeepAliver
	// announceC is event receives an event
	// whenever new announce has been sent, used in tests
	announceC chan struct{}
	// sendC is event channel used to trigger
	// new announces
	sendC chan struct{}
}

// Close closes all timers and goroutines
func (h *heartbeat) Close() error {
	// note that close does not clean up resources,
	// because it is unaware of heartbeat actual state,
	// run() could may as well be creating new keep aliver
	// while this function attempts to close it,
	// so instead it relies on run() loop to clean up after itself
	h.cancel()
	return nil
}

// ticker returns either fast ticker for keep alives
// or slow ticker for full server updates
// depending on the state
func (h *heartbeat) ticker() <-chan time.Time {
	if h.state == heartbeatStateKeepAlive {
		return h.keepAliveTicker.C
	}
	return h.announceTicker.C
}

// setState is used to debug state transitions
// as it logs in addition to setting state
func (h *heartbeat) setState(state keepAliveState) {
	h.state = state
	h.WithFields(log.Fields{"state": state.String()}).Debugf("Update state.")
}

// reset resets keep alive state
// and sends the state back to the initial state
// of sending full update
func (h *heartbeat) reset() {
	h.setState(heartbeatStateAnnounce)
	h.keepAlive = nil
	if h.keepAliver != nil {
		if err := h.keepAliver.Close(); err != nil {
			h.Warningf("Failed to close keep aliver: %v", err)
		}
		h.keepAliver = nil
	}
}

// fetch, if succeeded updates or sets current server
// to the last received server
func (h *heartbeat) fetch() error {
	// failed to fetch server info?
	// reset so next time full server update will be sent
	server, err := h.getServerInfo()
	if err != nil {
		h.reset()
		return trace.Wrap(err)
	}
	// no last server recorded? reset, so can send full update
	current := h.current
	h.current = server
	if current == nil {
		h.reset()
		return nil
	}
	result := services.CompareServers(server, current)
	// servers are different, reset keep alives and re-send full state
	if result == services.Different {
		h.reset()
		return nil
	}
	// keep whatever state is current, if keep alive is current
	// keep sending keep alives, otherwise update full node
	return nil
}

func (h *heartbeat) announce() error {
	switch h.state {
	case heartbeatStateAnnounce:
		// proxies don't support keep alive logic yet,
		// so keep state at announce forever for proxies
		if h.proxyMode {
			err := h.accessPoint.UpsertProxy(h.current)
			if err != nil {
				return trace.Wrap(err)
			}
			h.notifySend()
			return nil
		}
		keepAlive, err := h.accessPoint.UpsertNode(h.current)
		if err != nil {
			return trace.Wrap(err)
		}
		h.notifySend()
		keepAliver, err := h.accessPoint.NewKeepAliver(h.cancelCtx)
		if err != nil {
			h.reset()
			return trace.Wrap(err)
		}
		h.keepAlive = keepAlive
		h.keepAliver = keepAliver
		h.setState(heartbeatStateKeepAlive)
		return nil
	case heartbeatStateKeepAlive:
		keepAlive := *h.keepAlive
		keepAlive.Expires = h.clock.Now().UTC().Add(h.keepAlivePeriod)
		timeout := time.NewTimer(h.keepAlivePeriod)
		defer timeout.Stop()
		select {
		case <-h.cancelCtx.Done():
			return nil
		case <-timeout.C:
			h.Warningf("Blocked on keep alive send, going to reset.")
			h.reset()
			return trace.ConnectionProblem(nil, "timeout sending keep alive")
		case h.keepAliver.KeepAlives() <- keepAlive:
			h.notifySend()
			return nil
		case <-h.keepAliver.Done():
			h.Warningf("Keep alive has failed: %v", h.keepAliver.Error())
			err := h.keepAliver.Error()
			h.reset()
			return trace.ConnectionProblem(err, "keep alive channel closed")
		}
	default:
		return trace.BadParameter("unsupported state: %v", h.state)
	}
}

func (h *heartbeat) notifySend() {
	select {
	case h.announceC <- struct{}{}:
		return
	default:
	}
}

// fetchAndAnnounce fetches data about server
// and announces it to the server
func (h *heartbeat) fetchAndAnnounce() error {
	if err := h.fetch(); err != nil {
		return trace.Wrap(err)
	}
	if err := h.announce(); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// forceSend forces send cycle, used in tests, returns
// nil in case of success, error otherwise
func (h *heartbeat) forceSend(timeout time.Duration) error {
	timeoutC := time.After(timeout)
	select {
	case h.sendC <- struct{}{}:
	case <-timeoutC:
		return trace.ConnectionProblem(nil, "timeout waiting for send")
	}
	select {
	case <-h.announceC:
		return nil
	case <-timeoutC:
		return trace.ConnectionProblem(nil, "timeout waiting for announce to be sent")
	}
}

// run periodically calls to announce presence
func (h *heartbeat) run() {
	defer func() {
		h.reset()
		h.keepAliveTicker.Stop()
		h.announceTicker.Stop()
	}()
	for {
		if err := h.fetchAndAnnounce(); err != nil {
			h.Warningf("Heartbeat failed %v.", err)
		}
		select {
		case <-h.ticker():
		case <-h.sendC:
			h.Debugf("Asked to send out of cycle")
		case <-h.cancelCtx.Done():
			h.Debugf("Heartbeat exited.")
			return
		}
	}
}
