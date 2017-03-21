package middlewares

import (
	"net"
	"github.com/codegangsta/negroni"
	"net/http"
	"github.com/containous/traefik/log"
	"fmt"
)
// IpWhitelister is a middleware that provides Checks of the Requesting IP against a set of Whitelists
type IpWhitelister struct {
	handler   negroni.Handler
	whitelists []*net.IPNet
}

func NewIpWhitelister(whitelistStrings []string) (*IpWhitelister, error) {
	whitelister := IpWhitelister{}

	if(len(whitelistStrings) < 1) {
		return nil, fmt.Errorf("Error creating IpWhitelister: no whitelists provided")
	}

	whitelists := []*net.IPNet{}
	for _, whitelistString := range whitelistStrings {
		_, whitelist, err := net.ParseCIDR(whitelistString)
		if err != nil {
			return nil, fmt.Errorf("Error PArsing CIDR Whitelist %s: %s", whitelist, err)
		}
		whitelists = append(whitelists, whitelist)
	}

	whitelister.whitelists = whitelists
	whitelister.handler = negroni.HandlerFunc(func(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
		var match *net.IPNet = nil

		remoteIp, err := ipFromRemoteAddr(r.RemoteAddr)
		if err != nil {
			log.Warnf("Unable to parse Remote-Address from Header: %s - rejecting", r.RemoteAddr)
			reject(w)
		}

		for _, whitelist := range whitelists {
			if whitelist.Contains(*remoteIp) {
				match = whitelist
				break
			}
		}
		if match != nil {
			log.Debugf("Source-IP %s matched whitelist %s, passing", remoteIp, match)
			next.ServeHTTP(w, r)
		} else {
			log.Debugf("Source-IP %s matched none of the %d whitelists, rejecting", remoteIp, len(whitelists))
			reject(w)
		}
	})

	return &whitelister, nil
}
func reject(w http.ResponseWriter) {
	statusCode := http.StatusForbidden
	statusText := fmt.Sprintf("%d %s\n", http.StatusForbidden, http.StatusText(http.StatusForbidden))

	w.WriteHeader(statusCode)
	w.Write([]byte(statusText))
}
func ipFromRemoteAddr(addr string) (*net.IP, error) {
	ip, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("Can't extract IP/Port from address %s: %s", addr, err)
	}

	userIP := net.ParseIP(ip)
	if userIP == nil {
		return nil, fmt.Errorf("Can't parse IP from address %s", ip)
	}

	return &userIP, nil
}

func (a *IpWhitelister) ServeHTTP(rw http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	a.handler.ServeHTTP(rw, r, next)
}
