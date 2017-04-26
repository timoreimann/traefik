package server

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestNewServerWithoutWhitelistSourceRange(t *testing.T) {
	cases := []struct {
		desc                 string
		whitelistStrings     []string
		middlewareConfigured bool
		errMessage           string
	}{
		{
			desc:                 "no whitelists configued",
			whitelistStrings:     nil,
			middlewareConfigured: false,
			errMessage:           "",
		}, {
			desc: "whitelists configued",
			whitelistStrings: []string{
				"1.2.3.4/24",
				"fe80::/16",
			},
			middlewareConfigured: true,
			errMessage:           "",
		}, {
			desc: "invalid whitelists configued",
			whitelistStrings: []string{
				"foo",
			},
			middlewareConfigured: false,
			errMessage:           "parsing CIDR whitelist <nil>: invalid CIDR address: foo",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()
			middleware, err := configureIPWhitelistMiddleware(tc.whitelistStrings)

			if tc.errMessage != "" {
				assert.EqualError(t, err, tc.errMessage)
				return
			}
			assert.NoError(t, err)

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
