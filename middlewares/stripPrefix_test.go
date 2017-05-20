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

	// Note: This checks most-specific first, so we assume that a user
	//        sets rules at most-specific first when using PathPrefixStrip
	handler := &StripPrefix{
		Prefixes: []string{"/c/api/123/", "/a/api/", "/stat", "/"},
		Handler:  handlerPath,
	}

	server := httptest.NewServer(handler)
	defer server.Close()

	tests := []struct {
		expectedCode     int
		expectedResponse string
		url              string
	}{
		{url: "/", expectedCode: 200, expectedResponse: "/"},

		{url: "/stat", expectedCode: 200, expectedResponse: "/"},
		{url: "/stat/", expectedCode: 200, expectedResponse: "/"},
		{url: "/status", expectedCode: 200, expectedResponse: "/status"},
		{url: "/stat/us", expectedCode: 200, expectedResponse: "/us"},

		{url: "/a/test", expectedCode: 200, expectedResponse: "/a/test"},
		{url: "/a/api", expectedCode: 200, expectedResponse: "/a/api"},
		{url: "/a/api/", expectedCode: 200, expectedResponse: "/"},
		{url: "/a/api/test", expectedCode: 200, expectedResponse: "/test"},

		{url: "/c/api/123/", expectedCode: 200, expectedResponse: "/"},
		{url: "/c/api/123/test3", expectedCode: 200, expectedResponse: "/test3"},
		{url: "/c/api/abc/test4", expectedCode: 200, expectedResponse: "/c/api/abc/test4"},
	}

	for _, test := range tests {
		resp, err := http.Get(server.URL + test.url)
		if err != nil {
			t.Fatal(err)
		}

		if resp.StatusCode != test.expectedCode {
			t.Errorf("Received non-%d response: %d", test.expectedCode, resp.StatusCode)
		}
		response, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}

		if test.expectedResponse != string(response) {
			t.Errorf("Expected '%s' :  '%s'\n", test.expectedResponse, response)
		}
	}

}
