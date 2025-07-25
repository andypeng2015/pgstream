// SPDX-License-Identifier: Apache-2.0

package neosync

import (
	"context"

	"github.com/xataio/pgstream/pkg/transformers"
)

const (
	uppercaseLetters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	lowercaseLetters = "abcdefghijklmnopqrstuvwxyz"
)

// transformer is a wrapper around a neosync transformer. Neosync transformers
// return a pointer to the type, so this implementation is generic to ensure
// different types are supported.
type transformer[T any] struct {
	neosyncTransformer neosyncTransformer
	opts               any
}

type neosyncTransformer interface {
	Transform(value any, opts any) (any, error)
}

func New[T any](t neosyncTransformer, opts any) *transformer[T] {
	return &transformer[T]{
		opts:               opts,
		neosyncTransformer: t,
	}
}

func (t *transformer[T]) Transform(_ context.Context, value transformers.Value) (any, error) {
	retPtr, err := t.neosyncTransformer.Transform(value.TransformValue, t.opts)
	if err != nil {
		return nil, mapError(err)
	}

	ret, ok := retPtr.(*T)
	if !ok {
		return nil, transformers.ErrUnsupportedValueType
	}

	if ret == nil {
		return nil, nil
	}

	return *ret, nil
}

func findParameter[T any](params transformers.ParameterValues, name string) (*T, error) {
	var found bool
	var err error

	val := new(T)
	*val, found, err = transformers.FindParameter[T](params, name)
	if err != nil {
		return nil, err
	}
	if !found {
		val = nil
	}
	return val, nil
}

func findParameterArray[T any](params transformers.ParameterValues, name string) ([]T, error) {
	val, found, err := transformers.FindParameterArray[T](params, name)
	if err != nil {
		return val, err
	}
	if !found {
		val = nil
	}
	return val, nil
}

func toInt64Ptr(i *int) *int64 {
	if i == nil {
		return nil
	}

	i64 := int64(*i)
	return &i64
}

func toAnyPtr(strArray []string) *any {
	if len(strArray) == 0 {
		return nil
	}

	strArrayAny := any(strArray)
	return &strArrayAny
}
