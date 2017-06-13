// package roundrobin implements dynamic weighted round robin load balancer http handler
package roundrobin

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/vulcand/oxy/utils"
)

// Weight is an optional functional argument that sets weight of the server
func Weight(w int) ServerOption {
	return func(s *server) error {
		if w < 0 {
			return fmt.Errorf("Weight should be >= 0")
		}
		s.weight = w
		return nil
	}
}

// ErrorHandler is a functional argument that sets error handler of the server
func ErrorHandler(h utils.ErrorHandler) LBOption {
	return func(s *RoundRobin) error {
		s.errHandler = h
		return nil
	}
}

func EnableStickySession(ss *StickySession) LBOption {
	return func(s *RoundRobin) error {
		s.ss = ss
		return nil
	}
}

type RoundRobin struct {
	mutex      *sync.Mutex
	debugMutex *sync.Mutex
	next       http.Handler
	errHandler utils.ErrorHandler
	// Current index (starts from -1)
	index         int
	servers       []*server
	currentWeight int
	ss            *StickySession
}

func New(next http.Handler, opts ...LBOption) (*RoundRobin, error) {
	rr := &RoundRobin{
		next:       next,
		index:      -1,
		mutex:      &sync.Mutex{},
		debugMutex: &sync.Mutex{},
		servers:    []*server{},
		ss:         nil,
	}
	for _, o := range opts {
		if err := o(rr); err != nil {
			return nil, err
		}
	}
	if rr.errHandler == nil {
		rr.errHandler = utils.DefaultHandler
	}
	return rr, nil
}

func (r *RoundRobin) Next() http.Handler {
	return r.next
}

func (r *RoundRobin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.debugMutex.Lock()
	defer r.debugMutex.Unlock()

	// make shallow copy of request before chaning anything to avoid side effects
	newReq := *req
	stuck := false
	if r.ss != nil {
		fmt.Println("ServeHTTP: Sticky processing triggered")
		srvs := r.Servers()
		fmt.Printf("Getting backend for request URL %s (current number of servers: %d)\n", newReq.URL.String(), len(srvs))
		cookie_url, present, err := r.ss.GetBackend(&newReq, srvs)

		if err != nil {
			r.errHandler.ServeHTTP(w, req, err)
			return
		}

		if present {
			fmt.Printf("Cookie is present, URL is: %s\n", cookie_url)
			newReq.URL = cookie_url
			stuck = true
		} else {
			fmt.Println("cookie is NOT present.")
		}
	}

	if !stuck {
		url, err := r.NextServer()
		if err != nil {
			r.errHandler.ServeHTTP(w, req, err)
			return
		}

		if r.ss != nil {
			fmt.Printf("Sticking backend %s to response\n", url.String())
			r.ss.StickBackend(url, &w)
		}
		newReq.URL = url
	}
	fmt.Printf("Serving follow-up HTTP request in chain with request URL %s\n", newReq.URL.String())
	r.next.ServeHTTP(w, &newReq)
}

func (r *RoundRobin) NextServer() (*url.URL, error) {
	fmt.Println("Getting next server")
	srv, err := r.nextServer()
	if err != nil {
		return nil, err
	}
	return utils.CopyURL(srv.url), nil
}

func (r *RoundRobin) nextServer() (*server, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if len(r.servers) == 0 {
		return nil, fmt.Errorf("no servers in the pool")
	}

	// The algo below may look messy, but is actually very simple
	// it calculates the GCD  and subtracts it on every iteration, what interleaves servers
	// and allows us not to build an iterator every time we readjust weights

	// GCD across all enabled servers
	gcd := r.weightGcd()
	// Maximum weight across all enabled servers
	max := r.maxWeight()

	for {
		r.index = (r.index + 1) % len(r.servers)
		if r.index == 0 {
			r.currentWeight = r.currentWeight - gcd
			if r.currentWeight <= 0 {
				r.currentWeight = max
				if r.currentWeight == 0 {
					return nil, fmt.Errorf("all servers have 0 weight")
				}
			}
		}
		srv := r.servers[r.index]
		if srv.weight >= r.currentWeight {
			return srv, nil
		}
	}
	// We did full circle and found no available servers
	return nil, fmt.Errorf("no available servers")
}

func (r *RoundRobin) RemoveServer(u *url.URL) error {
	fmt.Printf("Removing server URL %s from pool\n", u.String())
	r.mutex.Lock()
	defer r.mutex.Unlock()

	e, index := r.findServerByURL(u)
	if e == nil {
		return fmt.Errorf("server not found")
	}
	r.servers = append(r.servers[:index], r.servers[index+1:]...)
	r.resetState()
	return nil
}

func (rr *RoundRobin) Servers() []*url.URL {
	rr.mutex.Lock()
	defer rr.mutex.Unlock()

	out := make([]*url.URL, len(rr.servers))
	for i, srv := range rr.servers {
		out[i] = srv.url
	}
	fmt.Printf("Returning %d servers.\n", len(out))
	return out
}

func (rr *RoundRobin) ServerWeight(u *url.URL) (int, bool) {
	rr.mutex.Lock()
	defer rr.mutex.Unlock()

	if s, _ := rr.findServerByURL(u); s != nil {
		return s.weight, true
	}
	return -1, false
}

// In case if server is already present in the load balancer, returns error
func (rr *RoundRobin) UpsertServer(u *url.URL, options ...ServerOption) error {
	fmt.Printf("Upserting server %s\n", u.String())
	rr.mutex.Lock()
	defer rr.mutex.Unlock()

	if u == nil {
		return fmt.Errorf("server URL can't be nil")
	}

	fmt.Printf("Trying to find server by URL %s\n", u.String())
	if s, _ := rr.findServerByURL(u); s != nil {
		fmt.Printf("Found server by URL %s\n", u.String())
		for _, o := range options {
			if err := o(s); err != nil {
				return err
			}
		}
		rr.resetState()
		return nil
	}

	fmt.Printf("Server URL %s not found -- Copying from URL\n", u.String())
	srv := &server{url: utils.CopyURL(u)}
	for _, o := range options {
		if err := o(srv); err != nil {
			return err
		}
	}

	if srv.weight == 0 {
		srv.weight = defaultWeight
	}

	fmt.Printf("Inserting upserted server URL %s\n", srv.url.String())
	rr.servers = append(rr.servers, srv)
	rr.resetState()
	return nil
}

func (r *RoundRobin) resetIterator() {
	r.index = -1
	r.currentWeight = 0
}

func (r *RoundRobin) resetState() {
	r.resetIterator()
}

func (r *RoundRobin) findServerByURL(u *url.URL) (*server, int) {
	fmt.Printf("Searching for server by URL %s in %d servers\n", u.String(), len(r.servers))
	if len(r.servers) == 0 {
		return nil, -1
	}
	for i, s := range r.servers {
		if sameURL(u, s.url) {
			fmt.Printf("Found server by URL %s\n", u.String())
			return s, i
		}
	}
	fmt.Printf("Could NOT find server by URL %s\n", u.String())
	return nil, -1
}

func (rr *RoundRobin) maxWeight() int {
	max := -1
	for _, s := range rr.servers {
		if s.weight > max {
			max = s.weight
		}
	}
	return max
}

func (rr *RoundRobin) weightGcd() int {
	divisor := -1
	for _, s := range rr.servers {
		if divisor == -1 {
			divisor = s.weight
		} else {
			divisor = gcd(divisor, s.weight)
		}
	}
	return divisor
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// ServerOption provides various options for server, e.g. weight
type ServerOption func(*server) error

// LBOption provides options for load balancer
type LBOption func(*RoundRobin) error

// Set additional parameters for the server can be supplied when adding server
type server struct {
	url *url.URL
	// Relative weight for the enpoint to other enpoints in the load balancer
	weight int
}

const defaultWeight = 1

func sameURL(a, b *url.URL) bool {
	res := a.Path == b.Path && a.Host == b.Host && a.Scheme == b.Scheme
	fmt.Printf("comparing URLs [P:%s H:%s S:%s] and [P:%s H:%s S:%s]: result = %t\n", a.Path, a.Host, a.Scheme, b.Path, b.Host, b.Scheme, res)
	return res
}

type balancerHandler interface {
	Servers() []*url.URL
	ServeHTTP(w http.ResponseWriter, req *http.Request)
	ServerWeight(u *url.URL) (int, bool)
	RemoveServer(u *url.URL) error
	UpsertServer(u *url.URL, options ...ServerOption) error
	NextServer() (*url.URL, error)
	Next() http.Handler
}
