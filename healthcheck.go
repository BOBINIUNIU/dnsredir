package redirect

import (
	"context"
	"crypto/tls"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
	"sync"
	"sync/atomic"
	"time"
)

// Transport settings
// Inspired from coredns/plugin/forward/persistent.go
// addr isn't sealed into this struct since it's a high-level item
type Transport struct {
	forceTcp  	bool				// forceTcp takes precedence over preferUdp
	preferUdp 	bool
	expire		time.Duration		// [sic] Expire (cached) connections after this time
	tlsConfig	*tls.Config
}

// UpstreamHostDownFunc can be used to customize how Down behaves
// see: proxy/healthcheck/healthcheck.go
type UpstreamHostDownFunc func(*UpstreamHost) bool

// UpstreamHost represents a single upstream DNS server
type UpstreamHost struct {
	// [PENDING]
	//protocol string					// DNS protocol, i.e. "udp", "tcp", "tcp-tls"
	addr string						// IP:PORT

	fails uint32					// Fail count
	downFunc UpstreamHostDownFunc	// This function should be side-effect save

	c *dns.Client					// DNS client used for health check

	// Transport settings related to this upstream host
	// Currently, it's the same as HealthCheck.transport since Caddy doesn't over nested blocks
	// XXX: We may support per-upstream specific transport once Caddy supported nesting blocks in future
	transport *Transport
}

func (uh *UpstreamHost) SetTLSConfig(config *tls.Config) {
	uh.c.Net = "tcp-tls"
	uh.c.TLSConfig = config
	uh.transport.tlsConfig = config
}

func (uh *UpstreamHost) Dial(proto string) (*dns.Conn, error) {
	switch {
	case uh.transport.tlsConfig != nil:
		proto = "tcp-tls"
	case uh.transport.forceTcp:
		proto = "tcp"
	case uh.transport.preferUdp:
		proto = "udp"
	}

	timeout := 1 * time.Second
	if proto == "tcp-tls" {
		return dns.DialTimeoutWithTLS(proto, uh.addr, uh.transport.tlsConfig, timeout)
	}
	return dns.DialTimeout(proto, uh.addr, timeout)
}

func (uh *UpstreamHost) Exchange(ctx context.Context, state request.Request) (*dns.Msg, error) {
	Unused(ctx)

	conn, err := uh.Dial(state.Proto())
	if err != nil {
		return nil, err
	}
	defer Close(conn)

	conn.UDPSize = uint16(state.Size())
	if conn.UDPSize < dns.MinMsgSize {
		conn.UDPSize = dns.MinMsgSize
	}

	_ = conn.SetWriteDeadline(time.Now().Add(defaultTimeout))
	if err := conn.WriteMsg(state.Req); err != nil {
		return nil, err
	}

	_ = conn.SetReadDeadline(time.Now().Add(defaultTimeout))
	return conn.ReadMsg()
}

// For health check we send to . IN NS +norec message to the upstream.
// Dial timeouts and empty replies are considered fails
// 	basically anything else constitutes a healthy upstream.
func (uh *UpstreamHost) Check() error {
	proto := "udp"
	if uh.c.Net != "" {
		proto = uh.c.Net
	}

	if err, _ := uh.send(); err != nil {
		atomic.AddUint32(&uh.fails, 1)
		log.Warningf("DNS @%v +%v dead?  err: %v", uh.addr, proto, err)
		return err
	} else {
		// Reset failure counter once health check success
		atomic.StoreUint32(&uh.fails, 0)
		//log.Infof("DNS @%v +%v ok  rtt: %v", uh.addr, proto, rtt)
		return nil
	}
}

func (uh *UpstreamHost) send() (error, time.Duration) {
	ping := &dns.Msg{}
	ping.SetQuestion(".", dns.TypeNS)

	// rtt stands for Round Trip Time
	msg, rtt, err := uh.c.Exchange(ping, uh.addr)
	// If we got a header, we're alright, basically only care about I/O errors 'n stuff.
	if err != nil && msg != nil {
		// Silly check, something sane came back.
		if msg.Response || msg.Opcode == dns.OpcodeQuery {
			proto := "udp"
			if uh.c.Net != "" {
				proto = uh.c.Net
			}
			log.Warningf("Correct DNS @%v +%v malformed response  err: %v msg: %v",
							uh.addr, proto, err, msg)
			err = nil
		}
	}

	return err, rtt
}

// UpstreamHostPool is an array of upstream DNS servers
type UpstreamHostPool []*UpstreamHost

// Down checks whether the upstream host is down or not
// Down will try to use uh.downFunc first, and will fallback
// 	to some default criteria if necessary.
func (uh *UpstreamHost) Down() bool {
	if uh.downFunc == nil {
		log.Warningf("Upstream host %v have no downFunc, fallback to default", uh.addr)
		fails := atomic.LoadUint32(&uh.fails)
		return fails > 0
	}

	down := uh.downFunc(uh)
	if down {
		log.Debugf("%v marked as down...", uh.addr)
	}
	return down
}

type HealthCheck struct {
	wg sync.WaitGroup		// Wait until all running goroutines to stop
	stopChan chan struct{}	// Signal health check worker to stop

	hosts UpstreamHostPool
	policy Policy
	spray Policy

	// [PENDING]
	//failTimeout time.Duration	// Single health check timeout

	maxFails uint32				// Maximum fail count considered as down
	checkInterval time.Duration	// Health check interval

	// A global transport since Caddy doesn't support over nested blocks
	transport *Transport
}

func (hc *HealthCheck) Start() {
	hc.stopChan = make(chan struct{})
	if hc.checkInterval != 0 {
		hc.wg.Add(1)
		go func() {
			defer hc.wg.Done()
			hc.healthCheckWorker()
		}()
	}
}

func (hc *HealthCheck) Stop() {
	close(hc.stopChan)
	hc.wg.Wait()
}

func (hc *HealthCheck) healthCheck() {
	for _, host := range hc.hosts {
		go host.Check()
	}
}

func (hc *HealthCheck) healthCheckWorker() {
	// Kick off initial health check immediately
	hc.healthCheck()

	ticker := time.NewTicker(hc.checkInterval)
	for {
		select {
		case <-ticker.C:
			hc.healthCheck()
		case <-hc.stopChan:
			return
		}
	}
}

// Select an upstream host based on the policy and the health check result
// Taken from proxy/healthcheck/healthcheck.go with modification
func (hc *HealthCheck) Select() *UpstreamHost {
	pool := hc.hosts
	if len(pool) == 1 {
		if pool[0].Down() && hc.spray == nil {
			return nil
		}
		return pool[0]
	}

	allDown := true
	for _, host := range pool {
		if !host.Down() {
			allDown = false
			break
		}
	}
	if allDown {
		if hc.spray == nil {
			return nil
		}
		return hc.spray.Select(pool)
	}

	if hc.policy == nil {
		// Default policy is random
		h := (&Random{}).Select(pool)
		if h != nil {
			return h
		}
		if hc.spray == nil {
			return nil
		}
		return hc.spray.Select(pool)
	}

	h := hc.policy.Select(pool)
	if h != nil {
		return h
	}

	if hc.spray == nil {
		return nil
	}
	return hc.spray.Select(pool)
}

