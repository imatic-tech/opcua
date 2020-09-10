// Copyright 2018-2020 opcua authors. All rights reserved.
// Use of this source code is governed by a MIT-style license that can be
// found in the LICENSE file.

package opcua

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gopcua/opcua/debug"
	"github.com/gopcua/opcua/errors"
	"github.com/gopcua/opcua/id"
	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uacp"
	"github.com/gopcua/opcua/uasc"
)

// GetEndpoints returns the available endpoint descriptions for the server.
func GetEndpoints(endpoint string) ([]*ua.EndpointDescription, error) {
	c := NewClient(endpoint, AutoReconnect(false))
	if err := c.Dial(context.Background()); err != nil {
		return nil, err
	}
	defer c.Close()
	res, err := c.GetEndpoints()
	if err != nil {
		return nil, err
	}
	return res.Endpoints, nil
}

// SelectEndpoint returns the endpoint with the highest security level which matches
// security policy and security mode. policy and mode can be omitted so that
// only one of them has to match.
// todo(fs): should this function return an error?
func SelectEndpoint(endpoints []*ua.EndpointDescription, policy string, mode ua.MessageSecurityMode) *ua.EndpointDescription {
	if len(endpoints) == 0 {
		return nil
	}

	sort.Sort(sort.Reverse(bySecurityLevel(endpoints)))
	policy = ua.FormatSecurityPolicyURI(policy)

	// don't care -> return highest security level
	if policy == "" && mode == ua.MessageSecurityModeInvalid {
		return endpoints[0]
	}

	for _, p := range endpoints {
		// match only security mode
		if policy == "" && p.SecurityMode == mode {
			return p
		}

		// match only security policy
		if p.SecurityPolicyURI == policy && mode == ua.MessageSecurityModeInvalid {
			return p
		}

		// match both
		if p.SecurityPolicyURI == policy && p.SecurityMode == mode {
			return p
		}
	}
	return nil
}

type bySecurityLevel []*ua.EndpointDescription

func (a bySecurityLevel) Len() int           { return len(a) }
func (a bySecurityLevel) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a bySecurityLevel) Less(i, j int) bool { return a[i].SecurityLevel < a[j].SecurityLevel }

// ConnState is the ua client connection state
type ConnState uint8

const (
	// Closed, the Connection is currently closed
	Closed ConnState = iota
	// Connected, the Connection is currently connected
	Connected
	// Connecting, the Connection is currently connecting to a server for the first time
	Connecting
	// Disconnected, the Connection is currently disconnected
	Disconnected
	// Reconnecting, the Connection is currently attempting to reconnect to a server it was previously connected to
	Reconnecting
)

// Client is a high-level client for an OPC/UA server.
// It establishes a secure channel and a session.
type Client struct {
	// endpointURL is the endpoint URL the client connects to.
	endpointURL string

	// cfg is the configuration for the secure channel.
	cfg *uasc.Config

	// sessionCfg is the configuration for the session.
	sessionCfg *uasc.SessionConfig

	// conn is the open connection
	conn *uacp.Conn

	// sechan is the open secure channel.
	sechan    *uasc.SecureChannel
	sechanErr chan error

	// session is the active session.
	session atomic.Value // *Session

	// subs is the set of active subscriptions by id.
	subs   map[uint32]*Subscription
	subMux sync.RWMutex

	// state of the client
	state atomic.Value // ConnState

	// monitorOnce ensures only one connection monitor is running
	monitorOnce sync.Once

	// sessionOnce initializes the session
	sessionOnce sync.Once
}

// NewClient creates a new Client.
//
// When no options are provided the new client is created from
// DefaultClientConfig() and DefaultSessionConfig(). If no authentication method
// is configured, a UserIdentityToken for anonymous authentication will be set.
// See #Client.CreateSession for details.
//
// To modify configuration you can provide any number of Options as opts. See
// #Option for details.
//
// https://godoc.org/github.com/gopcua/opcua#Option
func NewClient(endpoint string, opts ...Option) *Client {
	cfg, sessionCfg := ApplyConfig(opts...)
	c := Client{
		endpointURL: endpoint,
		cfg:         cfg,
		sessionCfg:  sessionCfg,
		sechanErr:   make(chan error, 1),
		subs:        make(map[uint32]*Subscription),
	}
	c.state.Store(Disconnected)
	return &c
}

// reconnectAction is a list of actions for the client reconnection logic.
type reconnectAction uint8

const (
	// none, no reconnection action
	none reconnectAction = iota
	// createSecureChannel, recreate secure channel action
	createSecureChannel
	// restoreSession, ask the server to repair session
	restoreSession
	// recreateSession, ask the client to repair session
	recreateSession
	// republishSubscriptions, ask the server to repair the previous subscription
	republishSubscriptions
	// restoreSubscriptions moves subscriptions from one session to another
	restoreSubscriptions
	// abortReconnect, the reconnecting is not possible
	abortReconnect
)

// Connect establishes a secure channel and creates a new session.
func (c *Client) Connect(ctx context.Context) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	if c.sechan != nil {
		return errors.Errorf("already connected")
	}

	c.state.Store(Connecting)
	if err := c.Dial(ctx); err != nil {
		return err
	}
	s, err := c.CreateSession(c.sessionCfg)
	if err != nil {
		_ = c.Close()
		return err
	}
	if err := c.ActivateSession(s); err != nil {
		_ = c.Close()
		return err
	}
	c.state.Store(Connected)

	c.monitorOnce.Do(func() {
		go c.monitor(ctx)
	})

	return nil
}

// monitor manages connection alteration
func (c *Client) monitor(ctx context.Context) {
	defer c.state.Store(Closed)

	action := none
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-c.sechanErr:
			// return if channel or connection is closed
			if !ok || err == io.EOF && c.State() == Closed {
				return
			}

			// tell the handler the connection is disconnected
			c.state.Store(Disconnected)

			if !c.cfg.AutoReconnect {
				// the connection is closed and should not be restored
				action = abortReconnect
				return
			}

			switch err {
			case io.EOF:
				// the connection has been closed
				action = createSecureChannel

			case syscall.ECONNREFUSED:
				// the connection has been refused by the server
				action = abortReconnect

			default:
				switch x := err.(type) {
				case *uacp.Error:
					switch ua.StatusCode(x.ErrorCode) {
					case ua.StatusBadSecureChannelIDInvalid:
						// the secure channel has been rejected by the server
						action = createSecureChannel

					case ua.StatusBadSessionIDInvalid:
						// the session has been rejected by the server
						action = recreateSession

					case ua.StatusBadSubscriptionIDInvalid:
						// the subscription has been rejected by the server
						action = restoreSubscriptions

					case ua.StatusBadCertificateInvalid:
						// todo(unknownet): recreate server certificate
						fallthrough

					default:
						// unknown error has occured
						action = createSecureChannel
					}

				default:
					// unknown error has occured
					action = createSecureChannel
				}
			}

			c.state.Store(Disconnected)
			c.pauseSubscriptions(ctx)

			for action != none {

				select {
				case <-ctx.Done():
					return

				default:

					switch action {
					case createSecureChannel:
						// recreate a secure channel by brute forcing
						// a reconnection to the server

						// close previous secure channel
						_ = c.conn.Close()
						c.sechan.Close()
						c.sechan = nil

						c.state.Store(Reconnecting)

						debug.Printf("Trying to recreate secure channel")
						for {
							if err := c.Dial(ctx); err != nil {
								select {
								case <-ctx.Done():
									return
								case <-time.After(c.cfg.ReconnectInterval):
									debug.Printf("Trying to recreate secure channel")
									continue
								}
							}
							break
						}
						debug.Printf("Secure channel recreated")
						action = restoreSession

					case restoreSession:
						// try to reactivate the session,
						// This only works if the session is still open on the server
						// otherwise recreate it

						if s := c.Session(); s == nil {
							// no session to restore
							action = recreateSession
							continue
						}

						debug.Printf("Trying to restore session")
						s, _ := c.DetachSession()
						if err := c.ActivateSession(s); err != nil {
							debug.Printf("Restore session failed")
							action = recreateSession
							continue
						}
						debug.Printf("Session restored")
						action = republishSubscriptions

					case recreateSession:
						// create a new session to replace the previous one

						debug.Printf("Trying to recreate session")
						s, err := c.CreateSession(c.sessionCfg)
						if err != nil {
							debug.Printf("Recreate session failed: %v", err)
							action = createSecureChannel
							continue
						}
						if err := c.ActivateSession(s); err != nil {
							debug.Printf("Reactivate session failed: %v", err)
							action = createSecureChannel
							continue
						}
						action = restoreSubscriptions

					case republishSubscriptions:
						// try to republish the previous subscriptions from the server
						// otherwise restore them

						// todo(fs): do we need this state at all?
						// todo(fs): isn't this path the same as restoreSubscriptions minus the transfer?
						// todo(fs): and if the transfer is a NoOp if the subscription is already on the
						// todo(fs): session then we can omit that or move it to a function which
						// todo(fs): handles the transfer or not.
						//
						// if err := c.republishSubscriptions(c.SubscriptionIDs()); err != nil {
						// 	debug.Printf("Republish subscription failed: %v", err)
						// 	action = createSecureChannel
						// 	continue
						// }

						var subsToRestore []uint32
						for _, id := range c.subscriptionIDs() {
							if err := c.republishSubscription(id); err != nil {
								debug.Printf("Republish of subscription %d failed", id)
								subsToRestore = append(subsToRestore, id)
							}
						}

						c.resumeSubscriptions(ctx)
						if len(subsToRestore) > 0 {
							if err := c.restoreSubscriptions(subsToRestore); err != nil {
								c.pauseSubscriptions(ctx)
								debug.Printf("Restore subscripitions failed: %v", err)
								action = recreateSession
								continue
							}
						}

						c.state.Store(Connected)
						action = none

					case restoreSubscriptions:
						// transfer subscriptions from the old to the new session
						// and try to republish the subscriptions.
						// Restore the subscriptions where republishing fails.

						subIDs := c.subscriptionIDs()
						subsToRepublish := []uint32{}
						subsToRestore := []uint32{}

						res, err := c.transferSubscriptions(subIDs)
						if err != nil {
							debug.Printf("Transfer subscriptions failed: %v", err)
							subsToRestore = subIDs
						} else {
							for i := range res.Results {
								transferResult := res.Results[i]
								if transferResult.StatusCode == ua.StatusBadSubscriptionIDInvalid {
									debug.Printf("Warning suscription %d should be recreated", subIDs[i])
									subsToRestore = append(subsToRestore, subIDs[i])
								} else {
									debug.Printf(
										"Subscription %d can be repaired with sequence number %d",
										subIDs[i],
										transferResult.AvailableSequenceNumbers[i],
									)
									subsToRepublish = append(subsToRepublish, subIDs[i])
								}
							}
						}

						// todo(fs): this looks wrong since we may be able to republish
						// todo(fs): some of the subscriptions but not all of them.
						// todo(fs): shouldn't we only restore the ones that failed to republish?
						// todo(fs): an alternative implementation is below
						//
						// if len(subsToRepublish) > 0 {
						// 	if err := c.republishSubscriptions(subsToRepublish); err != nil {
						// 		debug.Printf("Republish subscriptions failed: %v", err)
						// 		subsToRestore = append(subsToRestore, subsToRepublish...)
						// 	}
						// }

						for _, id := range subsToRepublish {
							if err := c.republishSubscription(id); err != nil {
								debug.Printf("Republish of subscription %d failed", id)
								subsToRestore = append(subsToRestore, id)
							}
						}

						c.resumeSubscriptions(ctx)
						if len(subsToRestore) > 0 {
							if err := c.restoreSubscriptions(subsToRestore); err != nil {
								c.pauseSubscriptions(ctx)
								debug.Printf("Restore subscripitions failed: %v", err)
								action = recreateSession
								continue
							}
						}

						c.state.Store(Connected)
						action = none

					case abortReconnect:
						// non recoverable disconnection
						// stop the client

						// todo(unknownet): should we store the error?
						debug.Printf("Reconnection not recoverable")
						return
					}
				}
			}

			// clear sechan errors from reconnection
			for len(c.sechanErr) > 0 {
				<-c.sechanErr
			}
		}
	}
}

// Dial establishes a secure channel.
func (c *Client) Dial(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	c.sessionOnce.Do(func() {
		c.session.Store((*Session)(nil))
	})

	if c.sechan != nil {
		return errors.Errorf("secure channel already connected")
	}

	var err error
	c.conn, err = uacp.Dial(ctx, c.endpointURL)
	if err != nil {
		return err
	}

	c.sechan, err = uasc.NewSecureChannel(c.endpointURL, c.conn, c.cfg, c.sechanErr)
	if err != nil {
		_ = c.conn.Close()
		return err
	}

	return c.sechan.Open(ctx)
}

// pauseSubscriptions pause all subscriptions
func (c *Client) pauseSubscriptions(ctx context.Context) {
	c.subMux.RLock()
	defer c.subMux.RUnlock()

	for _, sub := range c.subs {
		sub.pause(ctx)
	}
}

// pauseSubscriptions resume all subscriptions
func (c *Client) resumeSubscriptions(ctx context.Context) {
	c.subMux.RLock()
	defer c.subMux.RUnlock()

	for _, sub := range c.subs {
		sub.resume(ctx)
	}
}

// subscriptionIDs gets a list of subscriptionIDs
func (c *Client) subscriptionIDs() []uint32 {
	c.subMux.RLock()
	defer c.subMux.RUnlock()

	var ids []uint32
	for id := range c.subs {
		ids = append(ids, id)
	}
	return ids
}

// restoreSubscriptions creates new subscriptions
// with the same parameters to replace the previous ones
func (c *Client) restoreSubscriptions(ids []uint32) error {
	for _, id := range ids {
		if _, exist := c.subs[id]; !exist {
			debug.Printf("Cannot restore subscription %d", id)
			continue
		}

		sub := c.subs[id]

		debug.Printf("Restoring subscription %d", id)
		if err := sub.restore(); err != nil {
			debug.Printf("Restoring subscription %d failed", id)
			return err
		}
		debug.Printf("Restored subscription %d", id)
	}

	return nil
}

// transferSubscriptions ask the server to transfer the given subscriptions
// of the previous session to the current one.
func (c *Client) transferSubscriptions(ids []uint32) (*ua.TransferSubscriptionsResponse, error) {
	req := &ua.TransferSubscriptionsRequest{
		SubscriptionIDs:   ids,
		SendInitialValues: false,
	}

	var res *ua.TransferSubscriptionsResponse
	err := c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

// republishSubscriptions sends republish requests for all given subscription ids.
// func (c *Client) republishSubscriptions(ids []uint32) error {
// 	c.subMux.RLock()
// 	defer c.subMux.RUnlock()

// 	for _, id := range ids {
// 		sub, ok := c.subs[id]
// 		if !ok {
// 			return errors.Errorf("invalid subscription id %d", id)
// 		}
// 		if err := c.republishSubscription(sub); err != nil {
// 			return err
// 		}
// 	}

// 	return nil
// }

// republishSubscriptions sends republish requests for the given subscription id.
func (c *Client) republishSubscription(id uint32) error {

	// todo(fs): do we need to hold the lock for the entire time or only the map lookup?
	c.subMux.RLock()
	defer c.subMux.RUnlock()

	sub, ok := c.subs[id]
	if !ok {
		return errors.Errorf("invalid subscription id %d", id)
	}

	debug.Printf("republishing subscription %d", sub.SubscriptionID)
	if err := c.sendRepublishRequests(sub); err != nil {
		status, ok := err.(ua.StatusCode)
		if !ok {
			return err
		}

		switch status {
		case ua.StatusBadSessionIDInvalid:
			return nil
		case ua.StatusBadSubscriptionIDInvalid:
			// todo(fs): do we need to forget the subscription id in this case?
			debug.Printf("republish failed since subscription %d is invalid", sub.SubscriptionID)
			return errors.Errorf("republish failed since subscription %d is invalid", sub.SubscriptionID)
		}
	}
	return nil
}

// sendRepublishRequests sends republish requests for the given subscription
// until it gets a BadMessageNotAvailable which implies that there are no
// more messages to restore.
func (c *Client) sendRepublishRequests(sub *Subscription) error {
	seq := atomic.LoadUint32(&sub.lastSequenceNumber)
	defer atomic.StoreUint32(&sub.lastSequenceNumber, seq)

	for {
		req := &ua.RepublishRequest{
			SubscriptionID:           sub.SubscriptionID,
			RetransmitSequenceNumber: seq + 1,
		}
		seq++

		debug.Printf("Republishing subscription %d and sequence number %d",
			req.SubscriptionID,
			req.RetransmitSequenceNumber,
		)

		if c.sessionClosed() {
			debug.Printf("Republishing subscription %d aborted", req.SubscriptionID)
			return ua.StatusBadSessionClosed
		}

		res, err := sub.republish(req)
		if err != nil {
			if err == ua.StatusBadMessageNotAvailable {
				// No more message to restore
				debug.Printf("Republishing subscription %d OK", req.SubscriptionID)
				return nil
			}
			debug.Printf("Republishing subscription %d failed: %v", req.SubscriptionID, err)
			return err
		}

		status := ua.StatusBad
		if res != nil {
			status = res.ResponseHeader.ServiceResult
		}

		if status != ua.StatusOK {
			debug.Printf("Republishing subscription %d failed: %v", req.SubscriptionID, status)
			return status
		}
	}
}

// Close closes the session and the secure channel.
func (c *Client) Close() error {
	defer c.conn.Close()

	// try to close the session but ignore any error
	// so that we close the underlying channel and connection.
	_ = c.CloseSession()
	c.state.Store(Closed)
	defer close(c.sechanErr)
	if c.sechan != nil {
		_ = c.sechan.Close()
	}

	return nil
}

func (c *Client) State() ConnState {
	return c.state.Load().(ConnState)
}

// Session returns the active session.
func (c *Client) Session() *Session {
	return c.session.Load().(*Session)
}

func (c *Client) sessionClosed() bool {
	return c.Session() == nil
}

// Session is a OPC/UA session as described in Part 4, 5.6.
type Session struct {
	cfg *uasc.SessionConfig

	// resp is the response to the CreateSession request which contains all
	// necessary parameters to activate the session.
	resp *ua.CreateSessionResponse

	// serverCertificate is the certificate used to generate the signatures for
	// the ActivateSessionRequest methods
	serverCertificate []byte

	// serverNonce is the secret nonce received from the server during Create and Activate
	// Session response. Used to generate the signatures for the ActivateSessionRequest
	// and User Authorization
	serverNonce []byte
}

// CreateSession creates a new session which is not yet activated and not
// associated with the client. Call ActivateSession to both activate and
// associate the session with the client.
//
// If no UserIdentityToken is given explicitly before calling CreateSesion,
// it automatically sets anonymous identity token with the same PolicyID
// that the server sent in Create Session Response. The default PolicyID
// "Anonymous" wii be set if it's missing in response.
//
// See Part 4, 5.6.2
func (c *Client) CreateSession(cfg *uasc.SessionConfig) (*Session, error) {
	if c.sechan == nil {
		return nil, ua.StatusBadServerNotConnected
	}

	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	name := cfg.SessionName
	if name == "" {
		name = fmt.Sprintf("gopcua-%d", time.Now().UnixNano())
	}

	req := &ua.CreateSessionRequest{
		ClientDescription:       cfg.ClientDescription,
		EndpointURL:             c.endpointURL,
		SessionName:             name,
		ClientNonce:             nonce,
		ClientCertificate:       c.cfg.Certificate,
		RequestedSessionTimeout: float64(cfg.SessionTimeout / time.Millisecond),
	}

	var s *Session
	// for the CreateSessionRequest the authToken is always nil.
	// use c.sechan.Send() to enforce this.
	err := c.sechan.SendRequest(req, nil, func(v interface{}) error {
		var res *ua.CreateSessionResponse
		if err := safeAssign(v, &res); err != nil {
			return err
		}

		err := c.sechan.VerifySessionSignature(res.ServerCertificate, nonce, res.ServerSignature.Signature)
		if err != nil {
			log.Printf("error verifying session signature: %s", err)
			return nil
		}

		// Ensure we have a valid identity token that the server will accept before trying to activate a session
		if c.sessionCfg.UserIdentityToken == nil {
			opt := AuthAnonymous()
			opt(c.cfg, c.sessionCfg)

			p := anonymousPolicyID(res.ServerEndpoints)
			opt = AuthPolicyID(p)
			opt(c.cfg, c.sessionCfg)
		}

		s = &Session{
			cfg:               cfg,
			resp:              res,
			serverNonce:       res.ServerNonce,
			serverCertificate: res.ServerCertificate,
		}

		return nil
	})
	return s, err
}

const defaultAnonymousPolicyID = "Anonymous"

func anonymousPolicyID(endpoints []*ua.EndpointDescription) string {
	for _, e := range endpoints {
		if e.SecurityMode != ua.MessageSecurityModeNone || e.SecurityPolicyURI != ua.SecurityPolicyURINone {
			continue
		}

		for _, t := range e.UserIdentityTokens {
			if t.TokenType == ua.UserTokenTypeAnonymous {
				return t.PolicyID
			}
		}
	}

	return defaultAnonymousPolicyID
}

// ActivateSession activates the session and associates it with the client. If
// the client already has a session it will be closed. To retain the current
// session call DetachSession.
//
// See Part 4, 5.6.3
func (c *Client) ActivateSession(s *Session) error {
	if c.sechan == nil {
		return ua.StatusBadServerNotConnected
	}
	sig, sigAlg, err := c.sechan.NewSessionSignature(s.serverCertificate, s.serverNonce)
	if err != nil {
		log.Printf("error creating session signature: %s", err)
		return nil
	}

	switch tok := s.cfg.UserIdentityToken.(type) {
	case *ua.AnonymousIdentityToken:
		// nothing to do

	case *ua.UserNameIdentityToken:
		pass, passAlg, err := c.sechan.EncryptUserPassword(s.cfg.AuthPolicyURI, s.cfg.AuthPassword, s.serverCertificate, s.serverNonce)
		if err != nil {
			log.Printf("error encrypting user password: %s", err)
			return err
		}
		tok.Password = pass
		tok.EncryptionAlgorithm = passAlg

	case *ua.X509IdentityToken:
		tokSig, tokSigAlg, err := c.sechan.NewUserTokenSignature(s.cfg.AuthPolicyURI, s.serverCertificate, s.serverNonce)
		if err != nil {
			log.Printf("error creating session signature: %s", err)
			return err
		}
		s.cfg.UserTokenSignature = &ua.SignatureData{
			Algorithm: tokSigAlg,
			Signature: tokSig,
		}

	case *ua.IssuedIdentityToken:
		tok.EncryptionAlgorithm = ""
	}

	req := &ua.ActivateSessionRequest{
		ClientSignature: &ua.SignatureData{
			Algorithm: sigAlg,
			Signature: sig,
		},
		ClientSoftwareCertificates: nil,
		LocaleIDs:                  s.cfg.LocaleIDs,
		UserIdentityToken:          ua.NewExtensionObject(s.cfg.UserIdentityToken),
		UserTokenSignature:         s.cfg.UserTokenSignature,
	}
	return c.sechan.SendRequest(req, s.resp.AuthenticationToken, func(v interface{}) error {
		var res *ua.ActivateSessionResponse
		if err := safeAssign(v, &res); err != nil {
			return err
		}

		// save the nonce for the next request
		s.serverNonce = res.ServerNonce

		if err := c.CloseSession(); err != nil {
			// try to close the newly created session but report
			// only the initial error.
			_ = c.closeSession(s)
			return err
		}
		c.session.Store(s)
		return nil
	})
}

// CloseSession closes the current session.
//
// See Part 4, 5.6.4
func (c *Client) CloseSession() error {
	if err := c.closeSession(c.Session()); err != nil {
		return err
	}
	c.session.Store((*Session)(nil))
	return nil
}

// closeSession closes the given session.
func (c *Client) closeSession(s *Session) error {
	if s == nil {
		return nil
	}
	req := &ua.CloseSessionRequest{DeleteSubscriptions: true}
	var res *ua.CloseSessionResponse
	return c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
}

// DetachSession removes the session from the client without closing it. The
// caller is responsible to close or re-activate the session. If the client
// does not have an active session the function returns no error.
func (c *Client) DetachSession() (*Session, error) {
	s := c.Session()
	c.session.Store((*Session)(nil))
	return s, nil
}

// Send sends the request via the secure channel and registers a handler for
// the response. If the client has an active session it injects the
// authentication token.
func (c *Client) Send(req ua.Request, h func(interface{}) error) error {
	return c.sendWithTimeout(req, c.cfg.RequestTimeout, h)
}

// sendWithTimeout sends the request via the secure channel with a custom timeout and registers a handler for
// the response. If the client has an active session it injects the
// authentication token.
func (c *Client) sendWithTimeout(req ua.Request, timeout time.Duration, h func(interface{}) error) error {
	if c.sechan == nil {
		return ua.StatusBadServerNotConnected
	}
	var authToken *ua.NodeID
	if s := c.Session(); s != nil {
		authToken = s.resp.AuthenticationToken
	}
	return c.sechan.SendRequestWithTimeout(req, authToken, timeout, h)
}

// Node returns a node object which accesses its attributes
// through this client connection.
func (c *Client) Node(id *ua.NodeID) *Node {
	return &Node{ID: id, c: c}
}

func (c *Client) GetEndpoints() (*ua.GetEndpointsResponse, error) {
	req := &ua.GetEndpointsRequest{
		EndpointURL: c.endpointURL,
	}
	var res *ua.GetEndpointsResponse
	err := c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

// Read executes a synchronous read request.
//
// By default, the function requests the value of the nodes
// in the default encoding of the server.
func (c *Client) Read(req *ua.ReadRequest) (*ua.ReadResponse, error) {
	// clone the request and the ReadValueIDs to set defaults without
	// manipulating them in-place.
	rvs := make([]*ua.ReadValueID, len(req.NodesToRead))
	for i, rv := range req.NodesToRead {
		rc := &ua.ReadValueID{}
		*rc = *rv
		if rc.AttributeID == 0 {
			rc.AttributeID = ua.AttributeIDValue
		}
		if rc.DataEncoding == nil {
			rc.DataEncoding = &ua.QualifiedName{}
		}
		rvs[i] = rc
	}
	req = &ua.ReadRequest{
		MaxAge:             req.MaxAge,
		TimestampsToReturn: req.TimestampsToReturn,
		NodesToRead:        rvs,
	}

	var res *ua.ReadResponse
	err := c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

// Write executes a synchronous write request.
func (c *Client) Write(req *ua.WriteRequest) (*ua.WriteResponse, error) {
	var res *ua.WriteResponse
	err := c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

// Browse executes a synchronous browse request.
func (c *Client) Browse(req *ua.BrowseRequest) (*ua.BrowseResponse, error) {
	var res *ua.BrowseResponse
	err := c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

// Call executes a synchronous call request for a single method.
func (c *Client) Call(req *ua.CallMethodRequest) (*ua.CallMethodResult, error) {
	creq := &ua.CallRequest{
		MethodsToCall: []*ua.CallMethodRequest{req},
	}
	var res *ua.CallResponse
	err := c.Send(creq, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	if err != nil {
		return nil, err
	}
	if len(res.Results) != 1 {
		return nil, ua.StatusBadUnknownResponse
	}
	return res.Results[0], nil
}

// BrowseNext executes a synchronous browse request.
func (c *Client) BrowseNext(req *ua.BrowseNextRequest) (*ua.BrowseNextResponse, error) {
	var res *ua.BrowseNextResponse
	err := c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

// RegisterNodes registers node ids for more efficient reads.
// Part 4, Section 5.8.5
func (c *Client) RegisterNodes(req *ua.RegisterNodesRequest) (*ua.RegisterNodesResponse, error) {
	var res *ua.RegisterNodesResponse
	err := c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

// UnregisterNodes unregisters node ids previously registered with RegisterNodes.
// Part 4, Section 5.8.6
func (c *Client) UnregisterNodes(req *ua.UnregisterNodesRequest) (*ua.UnregisterNodesResponse, error) {
	var res *ua.UnregisterNodesResponse
	err := c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

// Subscribe creates a Subscription with given parameters. Parameters that have not been set
// (have zero values) are overwritten with default values.
// See opcua.DefaultSubscription* constants
func (c *Client) Subscribe(params *SubscriptionParameters, notifyCh chan *PublishNotificationData) (*Subscription, error) {
	if params == nil {
		params = &SubscriptionParameters{}
	}
	params.setDefaults()
	req := &ua.CreateSubscriptionRequest{
		RequestedPublishingInterval: float64(params.Interval / time.Millisecond),
		RequestedLifetimeCount:      params.LifetimeCount,
		RequestedMaxKeepAliveCount:  params.MaxKeepAliveCount,
		PublishingEnabled:           true,
		MaxNotificationsPerPublish:  params.MaxNotificationsPerPublish,
		Priority:                    params.Priority,
	}

	var res *ua.CreateSubscriptionResponse
	err := c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	if err != nil {
		return nil, err
	}
	if res.ResponseHeader.ServiceResult != ua.StatusOK {
		return nil, res.ResponseHeader.ServiceResult
	}

	sub := &Subscription{
		SubscriptionID:            res.SubscriptionID,
		RevisedPublishingInterval: time.Duration(res.RevisedPublishingInterval) * time.Millisecond,
		RevisedLifetimeCount:      res.RevisedLifetimeCount,
		RevisedMaxKeepAliveCount:  res.RevisedMaxKeepAliveCount,
		Notifs:                    notifyCh,
		params:                    params,
		publishch:                 make(chan publishReq), // must be unbuffered
		pausech:                   make(chan struct{}),   // must be unbuffered
		resumech:                  make(chan struct{}),   // must be unbuffered
		stopch:                    make(chan struct{}),   // must be unbuffered
		c:                         c,
	}

	c.subMux.Lock()
	if sub.SubscriptionID == 0 || c.subs[sub.SubscriptionID] != nil {
		// this should not happen and is usually indicative of a server bug
		// see: Part 4 Section 5.13.2.2, Table 88 – CreateSubscription Service Parameters
		c.subMux.Unlock()
		return nil, ua.StatusBadSubscriptionIDInvalid
	}
	c.subs[sub.SubscriptionID] = sub
	c.subMux.Unlock()

	return sub, nil
}

// registerSubscription register a subscription
func (c *Client) registerSubscription(sub *Subscription) error {
	if sub.SubscriptionID == 0 {
		return ua.StatusBadSubscriptionIDInvalid
	}

	c.subMux.Lock()
	defer c.subMux.Unlock()
	if _, ok := c.subs[sub.SubscriptionID]; ok {
		return errors.Errorf("SubscriptionID %d already registered", sub.SubscriptionID)
	}

	c.subs[sub.SubscriptionID] = sub
	return nil
}

func (c *Client) forgetSubscription(id uint32) {
	c.subMux.Lock()
	delete(c.subs, id)
	c.subMux.Unlock()
}

func (c *Client) notifySubscriptionsOfError(ctx context.Context, res *ua.PublishResponse, err error) {
	c.subMux.RLock()
	defer c.subMux.RUnlock()

	subsToNotify := c.subs
	if res != nil && res.SubscriptionID != 0 {
		subsToNotify = map[uint32]*Subscription{
			res.SubscriptionID: c.subs[res.SubscriptionID],
		}
	}
	for _, sub := range subsToNotify {
		go func(s *Subscription) {
			s.notify(ctx, &PublishNotificationData{Error: err})
		}(sub)
	}
}

func (c *Client) notifySubscription(ctx context.Context, response *ua.PublishResponse) {
	c.subMux.RLock()
	sub, ok := c.subs[response.SubscriptionID]
	c.subMux.RUnlock()
	if !ok {
		debug.Printf("Unknown subscription: %v", response.SubscriptionID)
		return
	}

	// todo(fs): response.Results contains the status codes of which messages were
	// todo(fs): were successfully removed from the transmission queue on the server.
	// todo(fs): The client sent the list of ids in the *previous* PublishRequest.
	// todo(fs): If we want to handle them then we probably need to keep track
	// todo(fs): of the message ids we have ack'ed.
	// todo(fs): see discussion in https://github.com/gopcua/opcua/issues/337

	if response.NotificationMessage == nil {
		sub.notify(ctx, &PublishNotificationData{
			SubscriptionID: response.SubscriptionID,
			Error:          errors.Errorf("empty NotificationMessage"),
		})
		return
	}

	// Part 4, 7.21 NotificationMessage
	for _, data := range response.NotificationMessage.NotificationData {
		// Part 4, 7.20 NotificationData parameters
		if data == nil || data.Value == nil {
			sub.notify(ctx, &PublishNotificationData{
				SubscriptionID: response.SubscriptionID,
				Error:          errors.Errorf("missing NotificationData parameter"),
			})
			continue
		}

		switch data.Value.(type) {
		// Part 4, 7.20.2 DataChangeNotification parameter
		// Part 4, 7.20.3 EventNotificationList parameter
		// Part 4, 7.20.4 StatusChangeNotification parameter
		case *ua.DataChangeNotification,
			*ua.EventNotificationList,
			*ua.StatusChangeNotification:
			sub.notify(ctx, &PublishNotificationData{
				SubscriptionID: response.SubscriptionID,
				Value:          data.Value,
			})

		// Error
		default:
			sub.notify(ctx, &PublishNotificationData{
				SubscriptionID: response.SubscriptionID,
				Error:          errors.Errorf("unknown NotificationData parameter: %T", data.Value),
			})
		}
	}
}

func (c *Client) HistoryReadRawModified(nodes []*ua.HistoryReadValueID, details *ua.ReadRawModifiedDetails) (*ua.HistoryReadResponse, error) {
	// Part 4, 5.10.3 HistoryRead
	req := &ua.HistoryReadRequest{
		TimestampsToReturn: ua.TimestampsToReturnBoth,
		NodesToRead:        nodes,
		// Part 11, 6.4 HistoryReadDetails parameters
		HistoryReadDetails: &ua.ExtensionObject{
			TypeID:       ua.NewFourByteExpandedNodeID(0, id.ReadRawModifiedDetails_Encoding_DefaultBinary),
			EncodingMask: ua.ExtensionObjectBinary,
			Value:        details,
		},
	}

	var res *ua.HistoryReadResponse
	err := c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

// safeAssign implements a type-safe assign from T to *T.
func safeAssign(t, ptrT interface{}) error {
	if reflect.TypeOf(t) != reflect.TypeOf(ptrT).Elem() {
		return InvalidResponseTypeError{t, ptrT}
	}

	// this is *ptrT = t
	reflect.ValueOf(ptrT).Elem().Set(reflect.ValueOf(t))
	return nil
}

type InvalidResponseTypeError struct {
	got, want interface{}
}

func (e InvalidResponseTypeError) Error() string {
	return fmt.Sprintf("invalid response: got %T want %T", e.got, e.want)
}
