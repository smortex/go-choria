package choria

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/choria-io/go-choria/backoff"
	"github.com/choria-io/go-protocol/protocol"
	"github.com/nats-io/go-nats"
	log "github.com/sirupsen/logrus"
)

// ConnectionManager is capable of being a factory for connection, mcollective.Choria is one
type ConnectionManager interface {
	NewConnector(ctx context.Context, servers func() ([]Server, error), name string, logger *log.Entry) (conn Connector, err error)
}

// PublishableConnector provides the minimal Connector features to enable publishing of choria.Message instances
type PublishableConnector interface {
	Publish(msg *Message) error
}

// AgentConnector provides the minimal Connector features for subscribing and unsubscribing agents
type AgentConnector interface {
	ConnectorInfo

	QueueSubscribe(ctx context.Context, name string, subject string, group string, output chan *ConnectorMessage) error
	Unsubscribe(name string) error
	AgentBroadcastTarget(collective string, agent string) string
}

type ClientConnector interface {
	PublishableConnector

	ReplyTarget(msg *Message) string
	ChanQueueSubscribe(name string, subject string, group string, capacity int) (chan *ConnectorMessage, error)
}

type ConnectorInfo interface {
	ConnectedServer() string
	ConnectionOptions() nats.Options
	ConnectionStats() nats.Statistics
}

type InstanceConnector interface {
	AgentConnector
	PublishableConnector

	NodeDirectedTarget(collective string, identity string) string

	Close()
}

// Connector is the interface a connector must implement to be valid be it NATS, Stomp, Testing etc
type Connector interface {
	InstanceConnector

	ReplyTarget(msg *Message) string
	ChanQueueSubscribe(name string, subject string, group string, capacity int) (chan *ConnectorMessage, error)
	PublishRaw(target string, data []byte) error
	Connect(ctx context.Context) (err error)
	Nats() *nats.Conn
}

type ConnectorMessage struct {
	Subject string
	Reply   string
	Data    []byte
}

type channelSubscription struct {
	subscription *nats.Subscription
	in           chan *nats.Msg
	out          chan *ConnectorMessage
	quit         chan interface{}
}

// Connection is a actual NATS connectoin handler, it implements Connector
type Connection struct {
	servers           func() ([]Server, error)
	name              string
	nats              *nats.Conn
	logger            *log.Entry
	choria            *Framework
	config            *Config
	subscriptions     map[string]*nats.Subscription
	chanSubscriptions map[string]*channelSubscription
	outbox            chan *nats.Msg
	subMu             sync.Mutex
	conMu             sync.Mutex
	recMu             sync.Mutex
}

var (
	connInitialConnectCtr = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "choria_connector_initial_connection_attempts",
		Help: "How many connection attempts were made before a connection was established",
	}, []string{"name"})

	connReconnectCtr = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "choria_connector_reconnections",
		Help: "Number of times the connector reconnected to the middleware",
	}, []string{"name"})

	connClosedCtr = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "choria_connector_connection_closed",
		Help: "Number of times the connection was closed",
	}, []string{"name"})

	connErrorCtr = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "choria_connector_errors",
		Help: "Number of times the connection encountered an error",
	}, []string{"name"})

	connInitialConnectTime = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "choria_connector_initial_connect_time",
		Help: "How long it took to establish the initial connection",
	}, []string{"name"})
)

func init() {
	prometheus.MustRegister(connInitialConnectCtr)
	prometheus.MustRegister(connReconnectCtr)
	prometheus.MustRegister(connClosedCtr)
	prometheus.MustRegister(connErrorCtr)
	prometheus.MustRegister(connInitialConnectTime)
}

// NewConnector creates a new NATS connector
//
// It will attempt to connect to the given servers and will keep trying till it manages to do so
func (fw *Framework) NewConnector(ctx context.Context, servers func() ([]Server, error), name string, logger *log.Entry) (conn Connector, err error) {
	if name == "" {
		name = fw.Config.Identity
	}

	conn = &Connection{
		name:              name,
		servers:           servers,
		logger:            logger,
		choria:            fw,
		config:            fw.Config,
		subscriptions:     make(map[string]*nats.Subscription),
		chanSubscriptions: make(map[string]*channelSubscription),
		outbox:            make(chan *nats.Msg, 1000),
	}

	err = conn.Connect(ctx)

	return conn, err
}

func (conn *Connection) ConnectionOptions() nats.Options {
	return conn.nats.Opts
}

func (conn *Connection) ConnectionStats() nats.Statistics {
	return conn.nats.Statistics
}

func (conn *Connection) Nats() *nats.Conn {
	return conn.nats
}

// ChanQueueSubscribe creates a channel of a certain size and subscribes to a queue group.
//
// The given name would later be used should a unsubscribe be needed
func (conn *Connection) ChanQueueSubscribe(name string, subject string, group string, capacity int) (chan *ConnectorMessage, error) {
	conn.subMu.Lock()
	defer conn.subMu.Unlock()

	var err error

	s := &channelSubscription{
		in:   make(chan *nats.Msg, capacity),
		out:  make(chan *ConnectorMessage, capacity),
		quit: make(chan interface{}),
	}

	conn.chanSubscriptions[name] = s

	conn.logger.Debugf("Susbscribing to %s in group '%s' on server %s", subject, group, conn.ConnectedServer())

	s.subscription, err = conn.nats.ChanQueueSubscribe(subject, group, s.in)
	if err != nil {
		return nil, fmt.Errorf("Could not subscribe to subscription %s: %s", name, err.Error())
	}

	copier := func(subs *channelSubscription) {
		for {
			select {
			case m := <-subs.in:
				subs.out <- &ConnectorMessage{Data: m.Data, Reply: m.Reply, Subject: m.Subject}
			case <-subs.quit:
				return
			}
		}
	}

	go copier(s)

	return s.out, nil
}

// QueueSubscribe is a lot like ChanQueueSubscribe but you provide it the queue to dump messages in
func (conn *Connection) QueueSubscribe(ctx context.Context, name string, subject string, group string, output chan *ConnectorMessage) error {
	conn.subMu.Lock()
	defer conn.subMu.Unlock()

	var err error

	s := &channelSubscription{
		in:   make(chan *nats.Msg, 5000),
		out:  output,
		quit: make(chan interface{}),
	}

	conn.chanSubscriptions[name] = s

	conn.logger.Debugf("Susbscribing to %s in group '%s' on server %s", subject, group, conn.ConnectedServer())

	s.subscription, err = conn.nats.ChanQueueSubscribe(subject, group, s.in)
	if err != nil {
		return fmt.Errorf("Could not subscribe to subscription %s: %s", name, err.Error())
	}

	copier := func(ctx context.Context, s *channelSubscription) {
		for {
			select {
			case m := <-s.in:
				s.out <- &ConnectorMessage{Data: m.Data, Reply: m.Reply, Subject: m.Subject}
			case <-ctx.Done():
				return
			case <-s.quit:
				return
			}
		}
	}

	go copier(ctx, s)

	return err
}

func (conn *Connection) Unsubscribe(name string) error {
	conn.subMu.Lock()
	defer conn.subMu.Unlock()

	if sub, ok := conn.subscriptions[name]; ok {
		err := sub.Unsubscribe()
		if err != nil {
			return fmt.Errorf("Could not unsubscribe from %s: %s", name, err.Error())
		}
	}

	if sub, ok := conn.chanSubscriptions[name]; ok {
		err := sub.subscription.Unsubscribe()
		if err != nil {
			return fmt.Errorf("Could not unsubscribe from %s: %s", name, err.Error())
		}

		close(sub.quit)
		close(sub.in)
		close(sub.out)
		delete(conn.chanSubscriptions, name)
	}

	return nil
}

// PublishRaw allows any data to be published to any target
func (conn *Connection) PublishRaw(target string, data []byte) error {
	log.Debugf("Publishing %d bytes to %s", len(data), target)

	return conn.nats.Publish(target, data)
}

// Publish inspects a Message and publish it according to its Type
func (conn *Connection) Publish(msg *Message) error {
	transport, err := msg.Transport()
	if err != nil {
		return fmt.Errorf("Cannot publish Message %s: %s", msg.RequestID, err.Error())
	}

	transport.RecordNetworkHop(conn.ConnectedServer(), conn.choria.Config.Identity, conn.ConnectedServer())

	if msg.CustomTarget != "" {
		return conn.publishConnectedBroadcast(msg, transport)
	}

	if conn.choria.IsFederated() {
		return conn.publishFederated(msg, transport)
	}

	return conn.publishConnected(msg, transport)
}

func (conn *Connection) publishFederated(msg *Message, transport protocol.TransportMessage) error {
	if msg.Type() == "direct_request" {
		return conn.publishFederatedDirect(msg, transport)
	}

	return conn.publishFederatedBroadcast(msg, transport)
}

func (conn *Connection) publishFederatedDirect(msg *Message, transport protocol.TransportMessage) error {
	var err error

	SliceGroups(msg.DiscoveredHosts, 200, func(nodes []string) {
		targets := []string{}
		var j string

		for i := range nodes {
			if nodes[i] == "" {
				break
			}

			targets = append(targets, conn.NodeDirectedTarget(msg.collective, nodes[i]))
		}

		transport.SetFederationRequestID(msg.RequestID)
		transport.SetFederationTargets(targets)

		j, err = transport.JSON()
		if err != nil {
			err = fmt.Errorf("Cannot publish Message %s: %s", msg.RequestID, err.Error())
			return
		}

		for _, federation := range conn.choria.FederationCollectives() {
			target := conn.federationTarget(federation, "federation")

			log.Debugf("Sending a federated direct message to NATS target '%s' for message %s with type %s", target, msg.RequestID, msg.Type())

			err = conn.PublishRaw(target, []byte(j))
			if err != nil {
				err = fmt.Errorf("Cannot publish Message %s: %s", msg.RequestID, err.Error())
				return
			}
		}
	})

	return err
}

func (conn *Connection) publishFederatedBroadcast(msg *Message, transport protocol.TransportMessage) error {
	target, err := conn.TargetForMessage(msg, "")
	if err != nil {
		return fmt.Errorf("Cannot publish Message %s: %s", msg.RequestID, err.Error())
	}

	transport.SetFederationRequestID(msg.RequestID)
	transport.SetFederationTargets([]string{target})

	j, err := transport.JSON()
	if err != nil {
		return fmt.Errorf("Cannot publish Message %s: %s", msg.RequestID, err.Error())
	}

	for _, federation := range conn.choria.FederationCollectives() {
		target := conn.federationTarget(federation, "federation")

		log.Debugf("Sending a federated broadcast message to NATS target '%s' for message %s with type %s", target, msg.RequestID, msg.Type())

		err = conn.PublishRaw(target, []byte(j))
		if err != nil {
			return fmt.Errorf("Cannot publish Message %s: %s", msg.RequestID, err.Error())
		}
	}

	return nil
}

func (conn *Connection) publishConnected(msg *Message, transport protocol.TransportMessage) error {
	if msg.Type() == "direct_request" {
		return conn.publishConnectedDirect(msg, transport)
	}

	return conn.publishConnectedBroadcast(msg, transport)
}

func (conn *Connection) publishConnectedBroadcast(msg *Message, transport protocol.TransportMessage) error {
	j, err := transport.JSON()
	if err != nil {
		return fmt.Errorf("Cannot publish Message %s: %s", msg.RequestID, err.Error())
	}

	target, err := conn.TargetForMessage(msg, "")
	if err != nil {
		return fmt.Errorf("Cannot publish Message %s: %s", msg.RequestID, err.Error())
	}

	log.Debugf("Sending a broadcast message to NATS target '%s' for message %s type %s", target, msg.RequestID, msg.Type())

	return conn.PublishRaw(target, []byte(j))

}

func (conn *Connection) publishConnectedDirect(msg *Message, transport protocol.TransportMessage) error {
	j, err := transport.JSON()
	if err != nil {
		return fmt.Errorf("Cannot publish Message %s: %s", msg.RequestID, err.Error())
	}

	rawmsg := []byte(j)

	for _, host := range msg.DiscoveredHosts {
		target, err := conn.TargetForMessage(msg, host)
		if err != nil {
			return fmt.Errorf("Cannot publish Message %s: %s", msg.RequestID, err.Error())
		}

		log.Debugf("Sending a direct message to %s via NATS target '%s' for message %s type %s", host, target, msg.RequestID, msg.Type())

		err = conn.PublishRaw(target, rawmsg)
		if err != nil {
			return fmt.Errorf("Could not publish directed message %s to %s: %s", msg.RequestID, host, err.Error())
		}
	}

	return nil
}

func (conn *Connection) TargetForMessage(msg *Message, identity string) (string, error) {
	if msg.CustomTarget != "" {
		return msg.CustomTarget, nil
	}

	if msg.Type() == "reply" {
		if msg.ReplyTo() == "" {
			return "", fmt.Errorf("Do not know how to reply, no reply-to header has been set on message %s", msg.RequestID)
		}

		return msg.ReplyTo(), nil

	} else if msg.Type() == "request" {
		return conn.AgentBroadcastTarget(msg.Collective(), msg.Agent), nil

	} else if msg.Type() == "direct_request" {
		return conn.NodeDirectedTarget(msg.Collective(), identity), nil
	}

	return "", fmt.Errorf("Do not know how to determine the target for Message %s with type %s", msg.RequestID, msg.Type())
}

func (conn *Connection) NodeDirectedTarget(collective string, identity string) string {
	return fmt.Sprintf("%s.node.%s", collective, identity)
}

func (conn *Connection) AgentBroadcastTarget(collective string, agent string) string {
	return fmt.Sprintf("%s.broadcast.agent.%s", collective, agent)
}

func (conn *Connection) ReplyTarget(msg *Message) string {
	return fmt.Sprintf("%s.reply.%s.%s", msg.Collective(), msg.SenderID, conn.choria.NewRequestID())
}

func (conn *Connection) federationTarget(federation string, side string) string {
	return fmt.Sprintf("choria.federation.%s.%s", federation, side)
}

// ConnectedServer returns the URL of the current server that the library is connected to, "unknown" when not initialized
func (conn *Connection) ConnectedServer() string {
	if conn.Nats() == nil {
		return "unknown"
	}

	url, err := url.Parse(conn.nats.ConnectedUrl())
	if err != nil {
		return "unknown"
	}

	return fmt.Sprintf("nats://%s:%s", strings.TrimSuffix(url.Hostname(), "."), url.Port())
}

// Connect creates a new connection to NATS.
//
// This will block until connected - basically forever should it never work.  Due to short comings
// in the NATS library logging about failures is not optimal
func (conn *Connection) Connect(ctx context.Context) (err error) {
	obs := prometheus.NewTimer(connInitialConnectTime.WithLabelValues(conn.name))
	defer obs.ObserveDuration()

	conn.conMu.Lock()
	defer conn.conMu.Unlock()

	var tlsc *tls.Config

	if !conn.choria.Config.DisableTLS {
		tlsc, err = conn.choria.TLSConfig()
		if err != nil {
			err = fmt.Errorf("Could not create TLS Config: %s", err.Error())
			return err
		}
	}

	urls := []string{}
	var url *url.URL

	servers, err := conn.servers()
	if err != nil {
		err = fmt.Errorf("Could not resolve servers during initial connect: %s", err.Error())
	}

	for _, server := range servers {
		url, err = server.URL()
		if err != nil {
			err = fmt.Errorf("Could not determine URL for server %#v", server)
			return
		}

		urls = append(urls, url.String())
	}

	options := []nats.Option{
		nats.MaxReconnects(-1),
		nats.Name(conn.name),
		nats.DisconnectHandler(func(nc *nats.Conn) {
			err = nc.LastError()

			if err != nil {
				conn.logger.Warnf("NATS client connection got disconnected: %s", nc.LastError())
			}
		}),

		nats.ReconnectHandler(func(nc *nats.Conn) {
			conn.logger.Warnf("NATS client reconnected after a previous disconnection, connected to %s", nc.ConnectedUrl())
			connReconnectCtr.WithLabelValues(conn.name).Inc()
		}),

		nats.ClosedHandler(func(nc *nats.Conn) {
			err = nc.LastError()
			if err != nil {
				conn.logger.Warnf("NATS client connection closed: %s", nc.LastError())
			}
			connClosedCtr.WithLabelValues(conn.name).Inc()
		}),

		nats.ErrorHandler(func(nc *nats.Conn, sub *nats.Subscription, err error) {
			conn.logger.Errorf("NATS client on %s encountered an error: %s", nc.ConnectedUrl(), err.Error())
			connErrorCtr.WithLabelValues(conn.name).Inc()
		}),
	}

	if !conn.choria.Config.DisableTLS {
		options = append(options, nats.Secure(tlsc))
	}

	try := 0

	for {
		try++

		conn.nats, err = nats.Connect(strings.Join(urls, ", "), options...)
		if err != nil {
			connInitialConnectCtr.WithLabelValues(conn.name).Inc()

			conn.logger.Warnf("Initial connection to the NATS broker cluster failed: %s", err.Error())

			if ctx.Err() != nil {
				err = fmt.Errorf("Initial connection cancelled due to shut down")
				return
			}

			s := backoff.FiveSec.Duration(try)
			conn.logger.Infof("Sleeping %s after failed connection attempt %d", s, try)
			time.Sleep(s)

			continue
		}

		conn.logger.Infof("Connected to %s", conn.nats.ConnectedUrl())

		break
	}

	return
}

// Close closes the NATS connection after flushing what needed to be sent
func (conn *Connection) Close() {
	for s := range conn.chanSubscriptions {
		conn.logger.Debugf("Stopping channel subscription %s", s)
		conn.chanSubscriptions[s].quit <- true
	}

	conn.logger.Debug("Flushing pending NATS messages before close")
	conn.nats.Flush()

	conn.logger.Debug("Closing NATS connection")
	conn.nats.Close()
}
