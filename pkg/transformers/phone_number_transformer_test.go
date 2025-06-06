// SPDX-License-Identifier: Apache-2.0

package transformers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhoneNumberTransformer_Transform(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		params        ParameterValues
		dynamicParams ParameterValues
		dynamicValues map[string]any
		value         any
		wantPrefix    string
		wantLen       int
		wantErr       error
	}{
		{
			name: "ok - string with prefix",
			params: ParameterValues{
				"prefix":     "(030) ",
				"min_length": 10,
				"max_length": 10,
			},
			value:      "12345",
			wantPrefix: "(030) ",
			wantLen:    10,
			wantErr:    nil,
		},
		{
			name: "ok - []byte without prefix",
			params: ParameterValues{
				"min_length": 6,
				"max_length": 6,
			},
			value:      []byte("12345"),
			wantPrefix: "",
			wantLen:    6,
			wantErr:    nil,
		},
		{
			name: "ok - []byte without prefix, deterministic generator",
			params: ParameterValues{
				"min_length": 6,
				"max_length": 6,
				"generator":  "deterministic",
			},
			value:      []byte("12345"),
			wantPrefix: "457059", // not prefix but the actual string, deterministic
			wantLen:    6,
			wantErr:    nil,
		},
		{
			name: "ok - with dynamic country code",
			params: ParameterValues{
				"min_length": 12,
				"max_length": 12,
			},
			dynamicParams: map[string]any{
				"prefix": map[string]any{
					"column": "country_code",
				},
			},
			dynamicValues: map[string]any{
				"country_code": "+90",
			},
			value:      "123456789",
			wantPrefix: "+90",
			wantLen:    12,
			wantErr:    nil,
		},
		{
			name: "error - prefix longer than min_length",
			params: ParameterValues{
				"prefix":     "12345678",
				"min_length": 6,
				"max_length": 10,
			},
			value:   "12345",
			wantErr: ErrInvalidParameters,
		},
		{
			name: "error - max_length less than min_length",
			params: ParameterValues{
				"min_length": 10,
				"max_length": 8,
			},
			value:   "12345",
			wantErr: ErrInvalidParameters,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transformer, err := NewPhoneNumberTransformer(tc.params, tc.dynamicParams)
			if tc.wantErr != nil {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			got, err := transformer.Transform(context.Background(), Value{TransformValue: tc.value, DynamicValues: tc.dynamicValues})
			require.NoError(t, err)

			gotStr, ok := got.(string)
			require.True(t, ok)
			require.Len(t, gotStr, tc.wantLen)
			if tc.wantPrefix != "" {
				require.True(t, len(gotStr) >= len(tc.wantPrefix))
				require.Equal(t, tc.wantPrefix, gotStr[:len(tc.wantPrefix)])
			}
		})
	}
}
