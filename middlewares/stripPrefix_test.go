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
		}
	}{
		{
			server: stripServers["/norules"],
			desc:   "no strip rules",
			tests: []struct {
				url      string
				expected string
			}{
				{
					url:      "/norules",
					expected: "/norules",
				},
			},
		},
		{
			server: stripServers["/"],
			desc:   "strip / to handle wildcard (.*) requests",
			tests: []struct {
				url      string
				expected string
			}{
				{
					url:      "/",
					expected: "/",
				},
			},
		},
		{
			server: stripServers["/stat/"],
			desc:   "strip /stat/ matching only subpaths",
			tests: []struct {
				url      string
				expected string
			}{
				{
					url:      "/stat",
					expected: "/stat",
				},
				{
					url:      "/stat/",
					expected: "/",
				},
				{
					url:      "/status",
					expected: "/status",
				},
				{
					url:      "/stat/us",
					expected: "/us",
				},
			},
		},
		{
			server: stripServers["/stat"],
			desc:   "strip /stat matching absolute paths and subpaths",
			tests: []struct {
				url      string
				expected string
			}{
				{
					url:      "/stat",
					expected: "/",
				},
				{
					url:      "/stat/",
					expected: "/",
				},
				{
					url:      "/status",
					expected: "/status",
				},
				{
					url:      "/stat/us",
					expected: "/us",
				},
			},
		},
		{
			server: stripServers["/nomatch"],
			desc:   "no matching strip rules",
			tests: []struct {
				url      string
				expected string
			}{
				{
					url:      "/anyurl",
					expected: "/anyurl",
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

				if resp.StatusCode != http.StatusOK {
					t.Errorf("Received non-200 response: %d", resp.StatusCode)
				}
				response, err := ioutil.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("Failed to read response body: %s", err)
				}

				if test.expected != string(response) {
					t.Errorf("Unexpected response received: '%s', expected: '%s'", response, test.expected)
				}
			}
		})
	}
}
