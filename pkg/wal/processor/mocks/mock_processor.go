// SPDX-License-Identifier: Apache-2.0

package mocks

import (
	"context"

	"github.com/xataio/pgstream/pkg/wal"
)

type Processor struct {
	ProcessWALEventFn func(ctx context.Context, walEvent *wal.Data) error
}

func (m *Processor) ProcessWALEvent(ctx context.Context, walEvent *wal.Data) error {
	return m.ProcessWALEventFn(ctx, walEvent)
}
