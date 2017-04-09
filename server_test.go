package main

import (
	"errors"
	"github.com/stretchr/testify/assert"
	"reflect"
	"testing"
)

func TestNewServerWithoutWhitelistSourceRange(t *testing.T) {
	cases := map[string]struct {
		whitelistStrings     []string
		middlewareConfigured bool
		err                  error
	}{
		"no whitelists configued": {
			whitelistStrings:     nil,
			middlewareConfigured: false,
			err:                  nil,
		},
		"whitelists configued": {
			whitelistStrings: []string{
				"1.2.3.4/24",
				"fe80::/16",
			},
			middlewareConfigured: true,
			err:                  nil,
		},
		"invalid whitelists configued": {
			whitelistStrings: []string{
				"foo",
			},
			middlewareConfigured: false,
			err:                  errors.New("parsing CIDR whitelist <nil>: invalid CIDR address: foo"),
		},
	}

	for index, e := range cases {
		t.Run(index, func(t *testing.T) {
			t.Parallel()
			middleware, err := configureIPWhitelistMiddleware(e.whitelistStrings)

			if !reflect.DeepEqual(err, e.err) {
				t.Errorf("expected err: %+v, got: %+v", e.err, err)
			}

			if e.middlewareConfigured {
				if !assert.NotNil(t, middleware, "not expected middleware to be configured") {
					return
				}
			} else {
				if !assert.Nil(t, middleware, "expected middleware to be configured") {
					return
				}
			}
		})
	}
}
