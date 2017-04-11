package main

import (
	"errors"
	"github.com/stretchr/testify/assert"
	"reflect"
	"testing"
)

func TestNewServerWithoutWhitelistSourceRange(t *testing.T) {
	cases := []struct {
		desc                 string
		whitelistStrings     []string
		middlewareConfigured bool
		err                  error
	}{
		{
			desc:                 "no whitelists configued",
			whitelistStrings:     nil,
			middlewareConfigured: false,
			err:                  nil,
		}, {
			desc: "whitelists configued",
			whitelistStrings: []string{
				"1.2.3.4/24",
				"fe80::/16",
			},
			middlewareConfigured: true,
			err:                  nil,
		}, {
			desc: "invalid whitelists configued",
			whitelistStrings: []string{
				"foo",
			},
			middlewareConfigured: false,
			err:                  errors.New("parsing CIDR whitelist <nil>: invalid CIDR address: foo"),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			middleware, err := configureIPWhitelistMiddleware(tc.whitelistStrings)

			if !reflect.DeepEqual(err, tc.err) {
				t.Errorf("expected err: %+v, got: %+v", tc.err, err)
			}

			if tc.middlewareConfigured {
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
