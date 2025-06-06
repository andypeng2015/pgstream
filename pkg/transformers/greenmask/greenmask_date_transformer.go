// SPDX-License-Identifier: Apache-2.0

package greenmask

import (
	"context"
	"fmt"
	"time"

	greenmasktransformers "github.com/eminano/greenmask/pkg/generators/transformers"
	"github.com/xataio/pgstream/pkg/transformers"
)

type DateTransformer struct {
	transformer *greenmasktransformers.Timestamp
}

var (
	dateParams = []transformers.Parameter{
		{
			Name:          "generator",
			SupportedType: "string",
			Default:       "random",
			Dynamic:       false,
			Required:      false,
			Values:        []any{"random", "deterministic"},
		},
		{
			Name:          "min_value",
			SupportedType: "string",
			Default:       nil,
			Dynamic:       false,
			Required:      true,
		},
		{
			Name:          "max_value",
			SupportedType: "string",
			Default:       nil,
			Dynamic:       false,
			Required:      true,
		},
	}
	dateCompatibleTypes = []transformers.SupportedDataType{
		transformers.StringDataType,
		transformers.ByteArrayDataType,
		transformers.DateDataType,
	}
)

func NewDateTransformer(params transformers.ParameterValues) (*DateTransformer, error) {
	minValue, foundMin, err := transformers.FindParameter[string](params, "min_value")
	if err != nil {
		return nil, fmt.Errorf("greenmask_date: min_value must be a string: %w", err)
	}

	maxValue, foundMax, err := transformers.FindParameter[string](params, "max_value")
	if err != nil {
		return nil, fmt.Errorf("greenmask_date: max_value must be a string: %w", err)
	}

	if !foundMin || !foundMax {
		return nil, errMinMaxValueNotSpecified
	}

	minTimestamp, err := time.Parse(time.DateOnly, minValue)
	if err != nil {
		return nil, fmt.Errorf("greenmask_date: min_value must be in yyyy-MM-dd format: %w", err)
	}

	maxTimestamp, err := time.Parse(time.DateOnly, maxValue)
	if err != nil {
		return nil, fmt.Errorf("greenmask_date: max_value must be in yyyy-MM-dd format: %w", err)
	}

	minTimestamp = time.Date(minTimestamp.Year(), minTimestamp.Month(), minTimestamp.Day(), 0, 0, 0, 0, time.UTC)
	maxTimestamp = time.Date(maxTimestamp.Year(), maxTimestamp.Month(), maxTimestamp.Day(), 23, 59, 59, 999999999, time.UTC)

	limiter, err := greenmasktransformers.NewTimestampLimiter(minTimestamp, maxTimestamp)
	if err != nil {
		return nil, err
	}

	t, err := greenmasktransformers.NewRandomTimestamp(greenmasktransformers.DayTruncateName, limiter)
	if err != nil {
		return nil, err
	}

	if err := setGenerator(t, params); err != nil {
		return nil, err
	}

	return &DateTransformer{
		transformer: t,
	}, nil
}

func (t *DateTransformer) Transform(_ context.Context, value transformers.Value) (any, error) {
	var toTransform []byte
	switch val := value.TransformValue.(type) {
	case time.Time:
		toTransform = []byte(val.Format(time.DateOnly))
	case []byte:
		toTransform = val
	case string:
		toTransform = []byte(val)
	default:
		return nil, transformers.ErrUnsupportedValueType
	}

	result, err := t.transformer.Transform(nil, toTransform)
	if err != nil {
		return nil, err
	}

	return time.Date(result.Year(), result.Month(), result.Day(), 0, 0, 0, 0, time.UTC), nil
}

func (t *DateTransformer) CompatibleTypes() []transformers.SupportedDataType {
	return dateCompatibleTypes
}

func (t *DateTransformer) Type() transformers.TransformerType {
	return transformers.GreenmaskDate
}

func DateTransformerDefinition() *transformers.Definition {
	return &transformers.Definition{
		SupportedTypes: dateCompatibleTypes,
		Parameters:     dateParams,
	}
}
