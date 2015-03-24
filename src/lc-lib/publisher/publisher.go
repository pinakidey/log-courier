/*
 * Copyright 2014 Jason Woods.
 *
 * This file is a modification of code from Logstash Forwarder.
 * Copyright 2012-2013 Jordan Sissel and contributors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package publisher

import (
	"errors"
	"fmt"
	"github.com/driskell/log-courier/src/lc-lib/core"
	"github.com/driskell/log-courier/src/lc-lib/registrar"
	"sync"
	"time"
)

var (
	ErrNetworkTimeout = errors.New("Server did not respond within network timeout")
	ErrNetworkPing    = errors.New("Server did not respond to keepalive")
)

const (
	// TODO(driskell): Make the idle timeout configurable like the network timeout is?
	keepalive_timeout time.Duration = 900 * time.Second
)

type TimeoutFunc func(*Publisher, *Endpoint)

type Publisher struct {
	core.PipelineSegment
	//core.PipelineConfigReceiver
	core.PipelineSnapshotProvider

	sync.RWMutex

	config           *core.NetworkConfig
	endpointSink     *EndpointSink

	firstPayload     *PendingPayload
	lastPayload      *PendingPayload
	numPayloads      int64
	outOfSync        int
	spoolChan        chan []*core.EventDescriptor
	registrarSpool   registrar.EventSpooler
	shuttingDown     bool

	line_count       int64
	line_speed       float64
	last_line_count  int64
	last_measurement time.Time
	seconds_no_ack   int

	timeoutTimer *time.Timer
	// TODO: Move these heads to EndpointSink
	timeoutHead  *Endpoint
	readyHead    *Endpoint
	fullHead     *Endpoint
	ifSpoolChan  <-chan []*core.EventDescriptor
	nextSpool    []*core.EventDescriptor
}

func NewPublisher(pipeline *core.Pipeline, config *core.NetworkConfig, registrar registrar.Registrator) *Publisher {
	ret := &Publisher{
		config: config,
		endpointSink: NewEndpointSink(config),
		spoolChan: make(chan []*core.EventDescriptor, 1),
		timeoutTimer: time.NewTimer(1 * time.Second),
	}

	ret.timeoutTimer.Stop()

	if registrar == nil {
		ret.registrarSpool = newNullEventSpool()
	} else {
		ret.registrarSpool = registrar.Connect()
	}

	// TODO: Option for round robin instead of load balanced?
	for _, server := range config.Servers {
		addressPool := NewAddressPool(server)
		ret.endpointSink.AddEndpoint(server, addressPool)
	}

	pipeline.Register(ret)

	return ret
}

func (p *Publisher) Connect() chan<- []*core.EventDescriptor {
	return p.spoolChan
}

func (p *Publisher) Run() {
	statsTimer := time.NewTimer(time.Second)
	onShutdown := p.OnShutdown()

	p.ifSpoolChan = p.spoolChan

PublishLoop:
	for {
		select {
		case endpoint := <-p.endpointSink.ReadyChan:
			p.registerReady(endpoint)
		case spool := <-p.ifSpoolChan:
			if p.readyHead != nil {
				log.Debug("[%s] %d new events queued, sending to endpoint", p.readyHead.Server(), len(spool))
				// We have a ready endpoint, send the spool to it
				p.readyHead.Ready = false
				p.sendPayload(p.readyHead, spool)
				p.readyHead = p.readyHead.NextReady
			} else {
				log.Debug("%d new events queued, awaiting endpoint readiness", len(spool))
				// No ready endpoint, wait for one
				p.nextSpool = spool
				p.ifSpoolChan = nil
			}
		case msg := <-p.endpointSink.ResponseChan:
			var err error
			switch msg.Response.(type) {
			case *AckResponse:
				err = p.processAck(msg.Endpoint(), msg.Response.(*AckResponse))
				if p.shuttingDown && p.numPayloads == 0 {
					log.Debug("Final ACK received, shutting down")
					break PublishLoop
				}
			case *PongResponse:
				err = p.processPong(msg.Endpoint(), msg.Response.(*PongResponse))
			default:
				err = fmt.Errorf("[%s] BUG ASSERTION: Unknown message type \"%T\"", msg.Endpoint().Server(), msg)
			}
			if err != nil {
				p.failEndpoint(msg.Endpoint(), err)
			}
		case failure := <-p.endpointSink.FailChan:
			p.failEndpoint(failure.Endpoint, failure.Error)
		case <-p.timeoutTimer.C:
			// Process triggered timers
			for {
				endpoint := p.timeoutHead
				p.timeoutHead = p.timeoutHead.NextTimeout

				callback := endpoint.TimeoutFunc
				endpoint.TimeoutFunc = nil
				log.Debug("[%s] Processing timeout", endpoint.Server())
				callback(p, endpoint)

				if p.timeoutHead == nil || p.timeoutHead.TimeoutDue.After(time.Now()) {
					break
				}
			}

			// Clear previous on the new head
			if p.timeoutHead != nil {
				p.timeoutHead.PrevTimeout = nil
				p.setTimer()
			}
		case <-statsTimer.C:
			p.updateStatistics()
			statsTimer.Reset(time.Second)
		case <-onShutdown:
			if p.numPayloads == 0 {
				log.Debug("Publisher has no outstanding payloads, shutting down")
				break PublishLoop
			}

			log.Warning("Publisher has outstanding payloads, waiting for responses before shutting down")
			onShutdown = nil
			p.ifSpoolChan = nil
			p.shuttingDown = true
		}
	}

	p.endpointSink.Shutdown()
	p.endpointSink.Wait()
	p.registrarSpool.Close()

	log.Info("Publisher exiting")

	p.Done()
}

func (p *Publisher) sendPayload(endpoint *Endpoint, events []*core.EventDescriptor) {
	// If this is the first payload, start the network timeout
	if endpoint.NumPending() == 0 {
		log.Debug("[%s] First payload, starting pending timeout", endpoint.Server())
		p.registerTimeout(endpoint, time.Now().Add(p.config.Timeout), (*Publisher).timeoutPending)
	}

	payload, err := NewPendingPayload(events)
	if err != nil {
		// TODO: Handle this
		return
	}

	if p.firstPayload == nil {
		p.firstPayload = payload
	} else {
		p.lastPayload.nextPayload = payload
	}
	p.lastPayload = payload

	p.Lock()
	p.numPayloads++
	p.Unlock()

	// TODO: Don't queue if send fails? Allows us to immediately resend from caller
	//       instead of waiting for failEndpoint to pull it back
	if err := endpoint.SendPayload(payload); err != nil {
		p.failEndpoint(endpoint, err)
	}
}

func (p *Publisher) processAck(endpoint *Endpoint, msg *AckResponse) error {
	payload, firstAck := endpoint.ProcessAck(msg)

	// We potentially receive out-of-order ACKs due to payloads distributed across servers
	// This is where we enforce ordering again to ensure registrar receives ACK in order
	if payload == p.firstPayload {
		// The out of sync count we have will never include the first payload, so
		// take the value +1
		outOfSync := p.outOfSync + 1

		// For each full payload we mark off, we decrease this count, the first we
		// mark off will always be the first payload - thus the +1. Subsequent
		// payloads are the out of sync ones - so if we mark them off we decrease
		// the out of sync count
		for payload.HasAck() {
			p.registrarSpool.Add(registrar.NewAckEvent(payload.Rollup()))

			if !payload.Complete() {
				break
			}

			payload = payload.nextPayload
			p.firstPayload = payload
			outOfSync--
			p.outOfSync = outOfSync

			p.Lock()
			p.numPayloads--
			p.Unlock()

			// TODO: Resume sending if we stopped due to excessive pending payload count
			//if !p.shutdown && p.can_send == nil {
			//	p.can_send = p.transport.CanSend()
			//}

			if payload == nil {
				break
			}
		}

		p.registrarSpool.Send()
	} else if firstAck {
		// If this is NOT the first payload, and this is the first acknowledgement
		// for this payload, then increase out of sync payload count
		p.outOfSync++
	}

	// Expect next ACK within network timeout if we still have pending
	if endpoint.NumPending() != 0 {
		log.Debug("[%s] Resetting pending timeout", endpoint.Server())
		p.registerTimeout(endpoint, time.Now().Add(p.config.Timeout), (*Publisher).timeoutPending)
	} else {
		log.Debug("[%s] Last payload acknowledged, starting keepalive timeout", endpoint.Server())
		p.registerTimeout(endpoint, time.Now().Add(keepalive_timeout), (*Publisher).timeoutKeepalive)
	}

	// If we're no longer full, move to ready queue
	// TODO: Use "peer send queue" - Move logic to EndpointSink
	if endpoint.Full && endpoint.NumPending() < 4 {
		log.Debug("[%s] Endpoint is no longer full (%d pending payloads)", endpoint.Server(), endpoint.NumPending())
		if endpoint.PrevFull == nil {
			p.fullHead = endpoint.NextFull
		} else {
			endpoint.PrevFull = endpoint.NextFull
		}
		if endpoint.NextFull != nil {
			endpoint.NextFull.PrevFull = endpoint.PrevFull
		}

		p.registerReady(endpoint)
	}

	return nil
}

func (p *Publisher) processPong(endpoint *Endpoint, msg *PongResponse) error {
	if err := endpoint.ProcessPong(); err != nil {
		return err
	}

	// If we haven't started sending anything, return to keepalive timeout
	if endpoint.NumPending() == 0 {
		log.Debug("[%s] Resetting keepalive timeout", endpoint.Server())
		p.registerTimeout(endpoint, time.Now().Add(p.config.Timeout), (*Publisher).timeoutKeepalive)
	}

	return nil
}

func (p *Publisher) failEndpoint(endpoint *Endpoint, err error) {
	log.Error("[%s] Endpoint failed: %s", endpoint.Server(), err)
	// TODO:
}

func (p *Publisher) registerReady(endpoint *Endpoint) {
	if endpoint.Ready {
		return
	}

	// TODO: Move logic to Endpoint/EndpointSink
	// TODO: Make configurable (bring back the "peer send queue" setting)
	if endpoint.NumPending() >= 4 {
		if endpoint.Full {
			return
		}

		log.Debug("[%s] Endpoint is full (%d pending payloads)", endpoint.Server(), endpoint.NumPending())

		endpoint.Full = true

		endpoint.PrevFull = nil
		endpoint.NextFull = p.fullHead
		if p.fullHead != nil {
			p.fullHead.PrevFull = endpoint
		}
		p.fullHead = endpoint
		return
	}

	if p.nextSpool != nil {
		log.Debug("[%s] Send is now ready, sending %d queued events", endpoint.Server(), len(p.nextSpool))
		// We have events, send it to the endpoint and wait for more
		p.sendPayload(endpoint, p.nextSpool)
		p.nextSpool = nil
		p.ifSpoolChan = p.spoolChan
	} else {
		log.Debug("[%s] Send is now ready, awaiting new events", endpoint.Server())
		// No events, save on the ready list and start the keepalive timer if none set
		p.addReady(endpoint)
		if endpoint.TimeoutFunc == nil {
			log.Debug("[%s] Starting keepalive timeout", endpoint.Server())
			p.registerTimeout(endpoint, time.Now().Add(keepalive_timeout), (*Publisher).timeoutKeepalive)
		}
	}
}

func (p *Publisher) addReady(endpoint *Endpoint) {
	// TODO: Move logic to EndpointSink
	endpoint.Ready = true

	// Least pending payloads connection takes preference
	next := p.readyHead

	if next == nil || next.NumPending() > endpoint.NumPending() {
		endpoint.NextReady = p.readyHead
		p.readyHead = endpoint
		return
	}

	var prev *Endpoint
	for prev, next = next, next.NextReady; next != nil; prev, next = next, next.NextReady {
		if next.NumPending() > endpoint.NumPending() {
			break
		}
	}

	prev.NextReady = endpoint
	endpoint.NextReady = next
}

func (p *Publisher) setTimer() {
	log.Debug("Timeout timer due at %v for %s", p.timeoutHead.TimeoutDue, p.timeoutHead.Server())
	p.timeoutTimer.Reset(p.timeoutHead.TimeoutDue.Sub(time.Now()))
}

func (p *Publisher) registerTimeout(endpoint *Endpoint, timeoutDue time.Time, timeoutFunc TimeoutFunc) {
	setTimer := false

	if endpoint.TimeoutFunc != nil {
		// Remove existing entry
		if endpoint.PrevTimeout == nil {
			p.timeoutHead = endpoint.NextTimeout
			setTimer = true
		} else {
			endpoint.PrevTimeout.NextTimeout = endpoint.NextTimeout
		}
		if endpoint.NextTimeout != nil {
			endpoint.NextTimeout.PrevTimeout = endpoint.PrevTimeout
		}
	}

	endpoint.TimeoutFunc = timeoutFunc
	endpoint.TimeoutDue = timeoutDue

	// Add to the list in time order
	next := p.timeoutHead

	if next == nil || next.TimeoutDue.After(timeoutDue) {
		p.timeoutHead = endpoint
		endpoint.PrevTimeout = nil
		endpoint.NextTimeout = next
		if next != nil {
			next.PrevTimeout = endpoint
		}
		p.setTimer()
		return
	}

	var prev *Endpoint
	for prev, next = next, next.NextTimeout; next != nil; prev, next = next, next.NextTimeout {
		if next.TimeoutDue.After(timeoutDue) {
			endpoint.PrevTimeout = prev
			break
		}
	}

	prev.NextTimeout = endpoint
	endpoint.PrevTimeout = prev
	endpoint.NextTimeout = next
	if next != nil {
		next.PrevTimeout = endpoint
	}

	if setTimer {
		p.setTimer()
	}
}

func (p *Publisher) timeoutPending(endpoint *Endpoint) {
	// Trigger a failure
	if endpoint.IsPinging() {
		p.failEndpoint(endpoint, ErrNetworkPing)
	} else {
		p.failEndpoint(endpoint, ErrNetworkTimeout)
	}
}

func (p *Publisher) timeoutKeepalive(endpoint *Endpoint) {
	// Timeout for PING
	log.Debug("[%s] Sending PING and starting pending timeout", endpoint.Server())
	p.registerTimeout(endpoint, time.Now().Add(p.config.Timeout), (*Publisher).timeoutPending)

	if err := endpoint.SendPing(); err != nil {
		p.failEndpoint(endpoint, err)
	}
}

func (p *Publisher) updateStatistics() {
	p.Lock()

	p.line_speed = core.CalculateSpeed(time.Since(p.last_measurement), p.line_speed, float64(p.line_count-p.last_line_count), &p.seconds_no_ack)

	p.last_line_count = p.line_count
	p.last_measurement = time.Now()

	p.Unlock()
}

func (p *Publisher) Snapshot() []*core.Snapshot {
	p.RLock()

	snapshot := core.NewSnapshot("Publisher")

	snapshot.AddEntry("Speed (Lps)", p.line_speed)
	snapshot.AddEntry("Published lines", p.last_line_count)
	snapshot.AddEntry("Pending Payloads", p.numPayloads)

	p.RUnlock()

	return []*core.Snapshot{snapshot}
}







/*
func (p *Publisher) RunOld() {
	defer func() {
		p.Done()
	}()

	var input_toggle <-chan []*core.EventDescriptor
	var retry_payload *pendingPayload
	var err error
	var reload int
	var hold bool

	timer := time.NewTimer(keepalive_timeout)
	stats_timer := time.NewTimer(time.Second)

	control_signal := p.OnShutdown()
	delay_shutdown := func() {
		// Flag shutdown for when we finish pending payloads
		// TODO: Persist pending payloads and resume? Quicker shutdown
		log.Warning("Delaying shutdown to wait for pending responses from the server")
		control_signal = nil
		p.shutdown = true
		p.can_send = nil
		input_toggle = nil
	}

PublishLoop:
	for {
		// Do we need to reload transport?
		if reload == core.Reload_Transport {
			// Shutdown and reload transport
			p.transport.Shutdown()

			if err = p.loadTransport(); err != nil {
				log.Error("The new transport configuration failed to apply: %s", err)
			}

			reload = core.Reload_None
		} else if reload != core.Reload_None {
			reload = core.Reload_None
		}

		if err = p.transport.Init(); err != nil {
			log.Error("Transport init failed: %s", err)

			now := time.Now()
			reconnect_due := now.Add(p.config.Reconnect)

		ReconnectTimeLoop:
			for {

				select {
				case <-time.After(reconnect_due.Sub(now)):
					break ReconnectTimeLoop
				case <-control_signal:
					// TODO: Persist pending payloads and resume? Quicker shutdown
					if p.num_payloads == 0 {
						break PublishLoop
					}

					delay_shutdown()
				case config := <-p.OnConfig():
					// Apply and check for changes
					reload = p.reloadConfig(&config.Network)

					// If a change and no pending payloads, process immediately
					if reload != core.Reload_None && p.num_payloads == 0 {
						break ReconnectTimeLoop
					}
				}

				now = time.Now()
				if now.After(reconnect_due) {
					break
				}
			}

			continue
		}

		p.Lock()
		p.status = Status_Connected
		p.Unlock()

		timer.Reset(keepalive_timeout)
		stats_timer.Reset(time.Second)

		p.pending_ping = false
		input_toggle = nil
		hold = false
		p.can_send = p.transport.CanSend()

	SelectLoop:
		for {
			select {
			case <-p.can_send:
				// Resend payloads from full retry first
				if retry_payload != nil {
					// Do we need to regenerate the payload?
					if retry_payload.payload == nil {
						if err = retry_payload.Generate(); err != nil {
							break SelectLoop
						}
					}

					// Reset timeout
					retry_payload.timeout = time.Now().Add(p.config.Timeout)

					log.Debug("Send now open: Retrying next payload")

					// Send the payload again
					if err = p.transport.Write("JDAT", retry_payload.payload); err != nil {
						break SelectLoop
					}

					// Expect an ACK within network timeout if this is the first of the retries
					if p.first_payload == retry_payload {
						timer.Reset(p.config.Timeout)
					}

					// Move to next non-empty payload
					for {
						retry_payload = retry_payload.next
						if retry_payload == nil || retry_payload.ack_events != len(retry_payload.events) {
							break
						}
					}

					break
				} else if p.out_of_sync != 0 {
					var resent bool
					if resent, err = p.checkResend(); err != nil {
						break SelectLoop
					} else if resent {
						log.Debug("Send now open: Resent a timed out payload")
						// Expect an ACK within network timeout
						timer.Reset(p.config.Timeout)
						break
					}
				}

				// No pending payloads, are we shutting down? Skip if so
				if p.shutdown {
					break
				}

				log.Debug("Send now open: Awaiting events for new payload")

				// Too many pending payloads, hold sending more until some are ACK
				if p.num_payloads >= p.config.MaxPendingPayloads {
					hold = true
				} else {
					input_toggle = p.input
				}
			case events := <-input_toggle:
				log.Debug("Sending new payload of %d events", len(events))

				// Send
				if err = p.sendNewPayload(events); err != nil {
					break SelectLoop
				}

				// Wait for send signal again
				input_toggle = nil

				if p.num_payloads >= p.config.MaxPendingPayloads {
					log.Debug("Pending payload limit of %d reached", p.config.MaxPendingPayloads)
				} else {
					log.Debug("%d/%d pending payloads now in transit", p.num_payloads, p.config.MaxPendingPayloads)
				}

				// Expect an ACK within network timeout if this is first payload after idle
				// Otherwise leave the previous timer
				if p.num_payloads == 1 {
					timer.Reset(p.config.Timeout)
				}
			case data := <-p.transport.Read():
				var signature, message []byte

				// Error? Or data?
				switch data.(type) {
				case error:
					err = data.(error)
					break SelectLoop
				default:
					signature = data.([][]byte)[0]
					message = data.([][]byte)[1]
				}

				switch {
				case bytes.Compare(signature, []byte("PONG")) == 0:
					if err = p.processPong(message); err != nil {
						break SelectLoop
					}
				case bytes.Compare(signature, []byte("ACKN")) == 0:
					if err = p.processAck(message); err != nil {
						break SelectLoop
					}
				default:
					err = fmt.Errorf("Unknown message received: % X", signature)
					break SelectLoop
				}

				// If no more pending payloads, set keepalive, otherwise reset to network timeout
				if p.num_payloads == 0 {
					// Handle shutdown
					if p.shutdown {
						break PublishLoop
					} else if reload != core.Reload_None {
						break SelectLoop
					}
					log.Debug("No more pending payloads, entering idle")
					timer.Reset(keepalive_timeout)
				} else {
					log.Debug("%d payloads still pending, resetting timeout", p.num_payloads)
					timer.Reset(p.config.Timeout)

					// Release any send hold if we're no longer at the max pending payloads
					if hold && p.num_payloads < p.config.MaxPendingPayloads {
						input_toggle = p.input
					}
				}
			case <-timer.C:
				// If we have pending payloads, we should've received something by now
				if p.num_payloads != 0 {
					err = ErrNetworkTimeout
					break SelectLoop
				}

				// If we haven't received a PONG yet this is a timeout
				if p.pending_ping {
					err = ErrNetworkPing
					break SelectLoop
				}

				log.Debug("Idle timeout: sending PING")

				// Send a ping and expect a pong back (eventually)
				// If we receive an ACK first, that's fine we'll reset timer
				// But after those ACKs we should get a PONG
				if err = p.transport.Write("PING", nil); err != nil {
					break SelectLoop
				}

				p.pending_ping = true

				// We may have just filled the send buffer
				input_toggle = nil

				// Allow network timeout to receive something
				timer.Reset(p.config.Timeout)
			case <-control_signal:
				// If no pending payloads, simply end
				if p.num_payloads == 0 {
					break PublishLoop
				}

				delay_shutdown()
			case config := <-p.OnConfig():
				// Apply and check for changes
				reload = p.reloadConfig(&config.Network)

				// If a change and no pending payloads, process immediately
				if reload != core.Reload_None && p.num_payloads == 0 {
					break SelectLoop
				}

				p.can_send = nil
			case <-stats_timer.C:
				p.updateStatistics(Status_Connected, nil)
				stats_timer.Reset(time.Second)
			}
		}

		if err != nil {
			// If we're shutting down and we hit a timeout and aren't out of sync
			// We can then quit - as we'd be resending payloads anyway
			if p.shutdown && p.out_of_sync == 0 {
				log.Error("Transport error: %s", err)
				break PublishLoop
			}

			p.updateStatistics(Status_Reconnecting, err)

			// An error occurred, reconnect after timeout
			log.Error("Transport error, will try again: %s", err)
			time.Sleep(p.config.Reconnect)
		} else {
			log.Info("Reconnecting transport")

			p.updateStatistics(Status_Reconnecting, nil)
		}

		retry_payload = p.first_payload
	}

	p.transport.Shutdown()

	// Disconnect from registrar
	p.registrar_spool.Close()

	log.Info("Publisher exiting")
}

func (p *Publisher) reloadConfig(new_config *core.NetworkConfig) int {
	old_config := p.config
	p.config = new_config

	// Transport reload will return whether we need a full reload or not
	reload := p.transport.ReloadConfig(new_config)
	if reload == core.Reload_Transport {
		return core.Reload_Transport
	}

	// Same servers?
	if len(new_config.Servers) != len(old_config.Servers) {
		return core.Reload_Servers
	}

	for i := range new_config.Servers {
		if new_config.Servers[i] != old_config.Servers[i] {
			return core.Reload_Servers
		}
	}

	return reload
}

func (p *Publisher) updateStatistics(status int, err error) {
	p.Lock()

	p.status = status

	p.line_speed = core.CalculateSpeed(time.Since(p.last_measurement), p.line_speed, float64(p.line_count-p.last_line_count), &p.seconds_no_ack)

	p.last_line_count = p.line_count
	p.last_retry_count = p.retry_count
	p.last_measurement = time.Now()

	if err == ErrNetworkTimeout || err == ErrNetworkPing {
		p.timeout_count++
	}

	p.Unlock()
}

func (p *Publisher) checkResend() (bool, error) {
	// We're out of sync (received ACKs for later payloads but not earlier ones)
	// Check timeouts of earlier payloads and resend if necessary
	if payload := p.first_payload; payload.timeout.Before(time.Now()) {
		p.retry_count++

		// Do we need to regenerate the payload?
		if payload.payload == nil {
			if err := payload.Generate(); err != nil {
				return false, err
			}
		}

		// Update timeout
		payload.timeout = time.Now().Add(p.config.Timeout)

		// Requeue the payload
		p.first_payload = payload.next
		payload.next = nil
		p.last_payload.next = payload
		p.last_payload = payload

		// Send the payload again
		if err := p.transport.Write("JDAT", payload.payload); err != nil {
			return false, err
		}

		return true, nil
	}

	return false, nil
}

func (p *Publisher) sendNewPayload(events []*core.EventDescriptor) (err error) {
	// Calculate a nonce
	nonce := p.generateNonce()
	for {
		if _, found := p.pending_payloads[nonce]; !found {
			break
		}
		// Collision - generate again - should be extremely rare
		nonce = p.generateNonce()
	}

	var payload *pendingPayload
	if payload, err = newPendingPayload(events, nonce, p.config.Timeout); err != nil {
		return
	}

	// Save pending payload until we receive ack, and discard buffer
	p.pending_payloads[nonce] = payload
	if p.first_payload == nil {
		p.first_payload = payload
	} else {
		p.last_payload.next = payload
	}
	p.last_payload = payload

	p.Lock()
	p.num_payloads++
	p.Unlock()

	return p.transport.Write("JDAT", payload.payload)
}

func (p *Publisher) processPong(message []byte) error {
	if len(message) != 0 {
		return fmt.Errorf("PONG message overflow (%d)", len(message))
	}

	// Were we pending a ping?
	if !p.pending_ping {
		return errors.New("Unexpected PONG received")
	}

	log.Debug("PONG message received")

	p.pending_ping = false
	return nil
}

func (p *Publisher) processAck(payload *pendingPayload, ackEvents int) {
	// We potentially receive out-of-order ACKs due to payloads distributed across servers
	// This is where we enforce ordering again to ensure registrar receives ACK in order
	if payload == p.firstPayload {
		// The out of sync count we have will never include the first payload, so
		// take the value +1
		outOfSync := p.outOfSync + 1

		// For each full payload we mark off, we decrease this count, the first we
		// mark off will always be the first payload - thus the +1. Subsequent
		// payloads are the out of sync ones - so if we mark them off we decrease
		// the out of sync count
		for payload.HasAck() {
			p.registrarSpool.Add(registrar.NewAckEvent(payload.Rollup()))

			if !payload.Complete() {
				break
			}

			payload = payload.next
			p.firstPayload = payload
			outOfSync--
			p.outOfSync = outOfSync

			p.Lock()
			p.numPayloads--
			p.Unlock()

			// Resume sending if we stopped due to excessive pending payload count
			if !p.shutdown && p.can_send == nil {
				p.can_send = p.transport.CanSend()
			}

			if payload == nil {
				break
			}
		}

		p.registrarSpool.Send()
	} else if ackEvents == 0 {
		// If this is NOT the first payload, and this is the first acknowledgement
		// for this payload, then increase out of sync payload count
		p.outOfSync++
	}
}*/
