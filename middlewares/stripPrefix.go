package middlewares

import (
	"net/http"
	"strings"
)

const (
	forwardedPrefixHeader = "X-Forwarded-Prefix"
)

// StripPrefix is a middleware used to strip prefix from an URL request
type StripPrefix struct {
	Handler  http.Handler
	Prefixes []string
}

func (s *StripPrefix) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, prefix := range s.Prefixes {
		orgPrefix := strings.TrimSpace(prefix)
		if orgPrefix == r.URL.Path {
			r.URL.Path = "/"
			s.serveRequest(w, r, orgPrefix)
			return
		}

		prefix = orgPrefix
		if !strings.HasSuffix(prefix, "/") {
			prefix = orgPrefix + "/"
		}

		if p := strings.TrimPrefix(r.URL.Path, prefix); len(p) < len(r.URL.Path) {
			if !strings.HasPrefix(p, "/") {
				r.URL.Path = "/" + p
			}
			s.serveRequest(w, r, orgPrefix)
			return
		}
	}
	http.NotFound(w, r)
}

func (s *StripPrefix) serveRequest(w http.ResponseWriter, r *http.Request, prefix string) {
	r.Header[forwardedPrefixHeader] = []string{prefix}
	r.RequestURI = r.URL.RequestURI()
	s.Handler.ServeHTTP(w, r)
}

// SetHandler sets handler
func (s *StripPrefix) SetHandler(Handler http.Handler) {
	s.Handler = Handler
}
