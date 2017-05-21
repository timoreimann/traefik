package middlewares

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	suites := []stripPrefixTestSuite{
		{
			server: stripServers["/norules"],
			desc:   "no strip rules",
			tests: []stripPrefixTestCase{
				{
					url:                "/norules",
					expectedStatusCode: http.StatusNotFound,
				},
			},
		},
		{
			server: stripServers["/"],
			desc:   "strip / to handle wildcard (.*) requests",
			tests: []stripPrefixTestCase{
				{
					url:                "/",
					expectedStatusCode: http.StatusOK,
					expectedBody:       "/",
				},
			},
		},
		{
			server: stripServers["/stat/"],
			desc:   "strip /stat/ matching only subpaths",
			tests: []stripPrefixTestCase{
				{
					url:                "/stat",
					expectedStatusCode: http.StatusNotFound,
				},
				{
					url:                "/stat/",
					expectedStatusCode: http.StatusOK,
					expectedBody:       "/",
				},
				{
					url:                "/status",
					expectedStatusCode: http.StatusNotFound,
				},
				{
					url:                "/stat/us",
					expectedStatusCode: http.StatusOK,
					expectedBody:       "/us",
				},
			},
		},
		{
			server: stripServers["/stat"],
			desc:   "strip /stat matching absolute paths and subpaths",
			tests: []stripPrefixTestCase{
				{
					url:                "/stat",
					expectedStatusCode: http.StatusOK,
					expectedBody:       "/",
				},
				{
					url:                "/stat/",
					expectedStatusCode: http.StatusOK,
					expectedBody:       "/",
				},
				{
					url:                "/status",
					expectedStatusCode: http.StatusNotFound,
				},
				{
					url:                "/stat/us",
					expectedStatusCode: http.StatusOK,
					expectedBody:       "/us",
				},
			},
		},
		{
			server: stripServers["/nomatch"],
			desc:   "no matching strip rules",
			tests: []stripPrefixTestCase{
				{
					url:                "/anyurl",
					expectedStatusCode: http.StatusNotFound,
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
				require.NoError(t, err, "Failed to send GET request")
				assert.Equal(t, test.expectedStatusCode, resp.StatusCode, "Unexpected status code")

				if test.expectedBody != "" {
					response, err := ioutil.ReadAll(resp.Body)
					require.NoError(t, err, "Failed to read response body")

					assert.Equal(t, test.expectedBody, string(response), "Unexpected response received")
				}
			}
		})
	}
}

type stripPrefixTestCase struct {
	url                string
	expectedStatusCode int
	expectedBody       string
}

type stripPrefixTestSuite struct {
	server *httptest.Server
	desc   string
	tests  []stripPrefixTestCase
}
