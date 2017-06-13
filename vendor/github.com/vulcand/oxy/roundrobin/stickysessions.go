// package stickysession is a mixin for load balancers that implements layer 7 (http cookie) session affinity
package roundrobin

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type StickySession struct {
	cookiename string
}

func NewStickySession(c string) *StickySession {
	return &StickySession{c}
}

// GetBackend returns the backend URL stored in the sticky cookie, iff the backend is still in the valid list of servers.
func (s *StickySession) GetBackend(req *http.Request, servers []*url.URL) (*url.URL, bool, error) {
	fmt.Printf("Looking up cookie by name %s\n", s.cookiename)
	cookie, err := req.Cookie(s.cookiename)
	cookieValues := req.Header["Cookie"]
	switch err {
	case nil:
		fmt.Printf("Cookie found by name %s (available cookies: [%s])\n", s.cookiename, strings.Join(cookieValues, ","))
	case http.ErrNoCookie:
		fmt.Printf("Could NOT find cookie by name %s (available cookies: %s)\n", s.cookiename, strings.Join(cookieValues, ","))
		return nil, false, nil
	default:
		return nil, false, err
	}

	fmt.Printf("Parsing cookie value %s\n", cookie.Value)
	s_url, err := url.Parse(cookie.Value)
	if err != nil {
		return nil, false, err
	}

	if s.isBackendAlive(s_url, servers) {
		return s_url, true, nil
	} else {
		return nil, false, nil
	}
}

func (s *StickySession) StickBackend(backend *url.URL, w *http.ResponseWriter) {
	c := &http.Cookie{Name: s.cookiename, Value: backend.String()}
	http.SetCookie(*w, c)
	return
}

func (s *StickySession) isBackendAlive(needle *url.URL, haystack []*url.URL) bool {
	fmt.Printf("Checking if backend for URL %s is alive (current pool size: %d)\n", needle.String(), len(haystack))

	if len(haystack) == 0 {
		fmt.Println("Pool is empty")
		return false
	}

	for _, s := range haystack {
		if sameURL(needle, s) {
			fmt.Printf("Found alive backend %s\n", s.String())
			return true
		}
	}
	fmt.Println("No alive backend found")
	return false
}
