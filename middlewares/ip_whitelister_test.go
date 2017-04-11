package middlewares

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/codegangsta/negroni"
	"github.com/stretchr/testify/assert"
)

func TestNewIPWhitelister(t *testing.T) {
	cases := map[string]struct {
		whitelistStrings   []string
		expectedWhitelists []*net.IPNet
		err                error
	}{
		"nil whitelist": {
			whitelistStrings:   nil,
			expectedWhitelists: nil,
			err:                errors.New("no whitelists provided"),
		},
		"empty whitelist": {
			whitelistStrings:   []string{},
			expectedWhitelists: nil,
			err:                errors.New("no whitelists provided"),
		},
		"whitelist containing empty string": {
			whitelistStrings: []string{
				"1.2.3.4/24",
				"",
				"fe80::/16",
			},
			expectedWhitelists: nil,
			err:                errors.New("parsing CIDR whitelist <nil>: invalid CIDR address: "),
		},
		"whitelist containing only an empty string": {
			whitelistStrings: []string{
				"",
			},
			expectedWhitelists: nil,
			err:                errors.New("parsing CIDR whitelist <nil>: invalid CIDR address: "),
		},
		"whitelist containing an invalid string": {
			whitelistStrings: []string{
				"foo",
			},
			expectedWhitelists: nil,
			err:                errors.New("parsing CIDR whitelist foo: invalid CIDR address: foo"),
		},
		"IPv4 & IPv6 whitelist": {
			whitelistStrings: []string{
				"1.2.3.4/24",
				"fe80::/16",
			},
			expectedWhitelists: []*net.IPNet{
				{IP: net.IPv4(1, 2, 3, 0), Mask: net.IPv4Mask(255, 255, 255, 0)},
				{IP: net.ParseIP("fe80::"), Mask: net.IPMask(net.ParseIP("ffff::"))},
			},
			err: nil,
		},
		"IPv4 only": {
			whitelistStrings: []string{
				"127.0.0.1/8",
			},
			expectedWhitelists: []*net.IPNet{
				{IP: net.IPv4(127, 0, 0, 0), Mask: net.IPv4Mask(255, 0, 0, 0)},
			},
			err: nil,
		},
	}

	for index, e := range cases {
		t.Run(index, func(t *testing.T) {
			t.Parallel()
			whitelister, err := NewIPWhitelister(e.whitelistStrings)
			if !reflect.DeepEqual(err, e.err) {
				t.Errorf("expected err: %+v, got: %+v", e.err, err)
				return
			}
			if e.err != nil {
				// expected error
				return
			}

			for index, actual := range whitelister.whitelists {
				expected := e.expectedWhitelists[index]
				if !actual.IP.Equal(expected.IP) || actual.Mask.String() != expected.Mask.String() {
					t.Errorf("unexpected result while comparing parsed ip whitelists, expected %s, got %s", actual, expected)
				}
			}
		})
	}
}

func TestIPWhitelisterHandle(t *testing.T) {
	cases := map[string]struct {
		whitelistStrings []string
		passIPs          []string
		rejectIPs        []string
	}{
		"IPv4": {
			whitelistStrings: []string{
				"1.2.3.4/24",
			},
			passIPs: []string{
				"1.2.3.1",
				"1.2.3.32",
				"1.2.3.156",
				"1.2.3.255",
			},
			rejectIPs: []string{
				"1.2.16.1",
				"1.2.32.1",
				"127.0.0.1",
				"8.8.8.8",
			},
		},
		"IPv4 sinlge IP": {
			whitelistStrings: []string{
				"8.8.8.8/32",
			},
			passIPs: []string{
				"8.8.8.8",
			},
			rejectIPs: []string{
				"8.8.8.7",
				"8.8.8.9",
				"8.8.8.0",
				"8.8.8.255",
				"4.4.4.4",
				"127.0.0.1",
			},
		},
		"multiple IPv4": {
			whitelistStrings: []string{
				"1.2.3.4/24",
				"8.8.8.8/8",
			},
			passIPs: []string{
				"1.2.3.1",
				"1.2.3.32",
				"1.2.3.156",
				"1.2.3.255",
				"8.8.4.4",
				"8.0.0.1",
				"8.32.42.128",
				"8.255.255.255",
			},
			rejectIPs: []string{
				"1.2.16.1",
				"1.2.32.1",
				"127.0.0.1",
				"4.4.4.4",
				"4.8.8.8",
			},
		},
		"IPv6": {
			whitelistStrings: []string{
				"2a03:4000:6:d080::/64",
			},
			passIPs: []string{
				"[2a03:4000:6:d080::]",
				"[2a03:4000:6:d080::1]",
				"[2a03:4000:6:d080:dead:beef:ffff:ffff]",
				"[2a03:4000:6:d080::42]",
			},
			rejectIPs: []string{
				"[2a03:4000:7:d080::]",
				"[2a03:4000:7:d080::1]",
				"[fe80::]",
				"[4242::1]",
			},
		},
		"IPv6 single IP": {
			whitelistStrings: []string{
				"2a03:4000:6:d080::42/128",
			},
			passIPs: []string{
				"[2a03:4000:6:d080::42]",
			},
			rejectIPs: []string{
				"[2a03:4000:6:d080::1]",
				"[2a03:4000:6:d080:dead:beef:ffff:ffff]",
				"[2a03:4000:6:d080::43]",
			},
		},
		"multiple IPv6": {
			whitelistStrings: []string{
				"2a03:4000:6:d080::/64",
				"fe80::/16",
			},
			passIPs: []string{
				"[2a03:4000:6:d080::]",
				"[2a03:4000:6:d080::1]",
				"[2a03:4000:6:d080:dead:beef:ffff:ffff]",
				"[2a03:4000:6:d080::42]",
				"[fe80::1]",
				"[fe80:aa00:00bb:4232:ff00:eeee:00ff:1111]",
				"[fe80::fe80]",
			},
			rejectIPs: []string{
				"[2a03:4000:7:d080::]",
				"[2a03:4000:7:d080::1]",
				"[4242::1]",
			},
		},
		"multiple IPv6 & IPv4": {
			whitelistStrings: []string{
				"2a03:4000:6:d080::/64",
				"fe80::/16",
				"1.2.3.4/24",
				"8.8.8.8/8",
			},
			passIPs: []string{
				"[2a03:4000:6:d080::]",
				"[2a03:4000:6:d080::1]",
				"[2a03:4000:6:d080:dead:beef:ffff:ffff]",
				"[2a03:4000:6:d080::42]",
				"[fe80::1]",
				"[fe80:aa00:00bb:4232:ff00:eeee:00ff:1111]",
				"[fe80::fe80]",
				"1.2.3.1",
				"1.2.3.32",
				"1.2.3.156",
				"1.2.3.255",
				"8.8.4.4",
				"8.0.0.1",
				"8.32.42.128",
				"8.255.255.255",
			},
			rejectIPs: []string{
				"[2a03:4000:7:d080::]",
				"[2a03:4000:7:d080::1]",
				"[4242::1]",
				"1.2.16.1",
				"1.2.32.1",
				"127.0.0.1",
				"4.4.4.4",
				"4.8.8.8",
			},
		},
		"broken IP-adresses": {
			whitelistStrings: []string{
				"127.0.0.1/32",
			},
			passIPs: nil,
			rejectIPs: []string{
				"foo",
				"10.0.0.350",
				"fe:::80",
				"",
				"\\&$§&/(",
			},
		},
	}

	for index, e := range cases {
		t.Run(index, func(t *testing.T) {
			//t.Parallel()
			whitelister, err := NewIPWhitelister(e.whitelistStrings)

			assert.NoError(t, err, "there should be no error")
			if !assert.NotNil(t, whitelister, "this should not be nil") {
				return
			}

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintln(w, "traefik")
			})
			n := negroni.New(whitelister)
			n.UseHandler(handler)

			for _, testIP := range e.passIPs {
				req, err := http.NewRequest("GET", "/", nil)
				assert.NoError(t, err)

				req.RemoteAddr = testIP + ":2342"
				recorder := httptest.NewRecorder()
				n.ServeHTTP(recorder, req)

				assert.Equal(t, http.StatusOK, recorder.Code, testIP+" should have passed "+index)
				assert.Contains(t, recorder.Body.String(), "traefik")
			}

			for _, testIP := range e.rejectIPs {
				req, err := http.NewRequest("GET", "/", nil)
				assert.NoError(t, err)

				req.RemoteAddr = testIP + ":2342"
				recorder := httptest.NewRecorder()
				n.ServeHTTP(recorder, req)

				assert.Equal(t, http.StatusForbidden, recorder.Code, testIP+" should not have passed "+index)
				assert.NotContains(t, recorder.Body.String(), "traefik")
			}
		})
	}
}
