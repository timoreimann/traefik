package middlewares

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStripPrefix(t *testing.T) {
	handlerPath := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.URL.Path)
	})

	stripServers := make(map[string]*httptest.Server)
	stripServers["/norules"] = httptest.NewServer(&StripPrefix{
		Prefixes: []string{},
		Handler:  handlerPath,
	})

	stripList := []string{
		"/",
		"/stat/",
		"/stat",
		"/nomatch",
	}
	for _, stripPath := range stripList {
		stripServers[stripPath] = httptest.NewServer(&StripPrefix{
			Prefixes: []string{stripPath},
			Handler:  handlerPath,
		})
	}

	suites := []struct {
		server *httptest.Server
		desc   string
		tests  []struct {
			url      string
			expected string
			code     int
		}
	}{
		{
			server: stripServers["/norules"],
			desc:   "no strip rules",
			tests: []struct {
				url      string
				expected string
				code     int
			}{
				{
					url:      "/norules",
					expected: "",
					code:     http.StatusNotFound,
				},
			},
		},
		{
			server: stripServers["/"],
			desc:   "strip / to handle wildcard (.*) requests",
			tests: []struct {
				url      string
				expected string
				code     int
			}{
				{
					url:      "/",
					expected: "/",
					code:     http.StatusOK,
				},
			},
		},
		{
			server: stripServers["/stat/"],
			desc:   "strip /stat/ matching only subpaths",
			tests: []struct {
				url      string
				expected string
				code     int
			}{
				{
					url:      "/stat",
					expected: "",
					code:     http.StatusNotFound,
				},
				{
					url:      "/stat/",
					expected: "/",
					code:     http.StatusOK,
				},
				{
					url:      "/status",
					expected: "",
					code:     http.StatusNotFound,
				},
				{
					url:      "/stat/us",
					expected: "/us",
					code:     http.StatusOK,
				},
			},
		},
		{
			server: stripServers["/stat"],
			desc:   "strip /stat matching absolute paths and subpaths",
			tests: []struct {
				url      string
				expected string
				code     int
			}{
				{
					url:      "/stat",
					expected: "/",
					code:     http.StatusOK,
				},
				{
					url:      "/stat/",
					expected: "/",
					code:     http.StatusOK,
				},
				{
					url:      "/status",
					expected: "",
					code:     http.StatusNotFound,
				},
				{
					url:      "/stat/us",
					expected: "/us",
					code:     http.StatusOK,
				},
			},
		},
		{
			server: stripServers["/nomatch"],
			desc:   "no matching strip rules",
			tests: []struct {
				url      string
				expected string
				code     int
			}{
				{
					url:      "/anyurl",
					expected: "",
					code:     http.StatusNotFound,
				},
			},
		},
	}

	for _, suite := range suites {
		suite := suite
		t.Run(suite.desc, func(t *testing.T) {
			t.Parallel()
			defer suite.server.Close()

			for _, test := range suite.tests {
				resp, err := http.Get(suite.server.URL + test.url)
				if err != nil {
					t.Fatalf("Failed to send GET request: %s", err)
				}

				if resp.StatusCode != test.code {
					t.Errorf("Received non-%d response: %d", test.code, resp.StatusCode)
				}
				response, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("Failed to read response body: %s", err)
				}

				if test.expected != "" && test.expected != string(response) {
					t.Errorf("Unexpected response received: '%s', expected: '%s'", response, test.expected)
				}
			}
		})
	}
}
