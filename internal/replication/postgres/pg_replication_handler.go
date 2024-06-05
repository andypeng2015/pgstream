// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/xataio/pgstream/internal/replication"
	loglib "github.com/xataio/pgstream/pkg/log"
)

type Handler struct {
	logger loglib.Logger
	// Create two connections. One for querying, one for handling replication
	// events.
	pgConn            *pgx.Conn
	pgReplicationConn *pgconn.PgConn

	pgReplicationSlotName string

	lsnParser replication.LSNParser
}

type Config struct {
	PostgresURL string
}

type Option func(h *Handler)

const (
	logLSNPosition = "position"
	logSlotName    = "slot_name"
	logTimeline    = "timeline"
	logDBName      = "db_name"
	logSystemID    = "system_id"
)

func NewHandler(ctx context.Context, cfg Config, opts ...Option) (*Handler, error) {
	pgCfg, err := pgx.ParseConfig(cfg.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("failed parsing postgres connection string: %w", err)
	}
	pgConn, err := pgx.ConnectConfig(ctx, pgCfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres client: %w", err)
	}

	// open a second Postgres connection, this one dedicated for replication
	copyConfig := pgCfg.Copy()
	copyConfig.RuntimeParams["replication"] = "database"

	pgReplicationConn, err := pgconn.ConnectConfig(context.Background(), &copyConfig.Config)
	if err != nil {
		return nil, fmt.Errorf("create postgres replication client: %w", err)
	}

	h := &Handler{
		logger:            loglib.NewNoopLogger(),
		pgConn:            pgConn,
		pgReplicationConn: pgReplicationConn,
		lsnParser:         &LSNParser{},
	}

	for _, opt := range opts {
		opt(h)
	}

	return h, nil
}

func WithLogger(l loglib.Logger) Option {
	return func(h *Handler) {
		h.logger = loglib.NewLogger(l)
	}
}

func (h *Handler) StartReplication(ctx context.Context) error {
	sysID, err := pglogrepl.IdentifySystem(ctx, h.pgReplicationConn)
	if err != nil {
		return fmt.Errorf("identifySystem failed: %w", err)
	}

	h.pgReplicationSlotName = fmt.Sprintf("pgstream_%s_slot", sysID.DBName)

	logFields := loglib.Fields{
		logSystemID: sysID.SystemID,
		logDBName:   sysID.DBName,
		logSlotName: h.pgReplicationSlotName,
	}
	h.logger.Info("replication handler: identifySystem success", logFields, loglib.Fields{
		logTimeline:    sysID.Timeline,
		logLSNPosition: sysID.XLogPos,
	})

	startPos, err := h.getLastSyncedLSN(ctx)
	if err != nil {
		return fmt.Errorf("read last position: %w", err)
	}

	h.logger.Trace("replication handler: read last LSN position", logFields, loglib.Fields{
		logLSNPosition: pglogrepl.LSN(startPos),
	})

	if startPos == 0 {
		// todo(deverts): If we don't have a position. Read from as early as possible.
		// this _could_ be too old. In the future, it would be good to calculate if we're
		// too far behind, so we can fix it.
		startPos, err = h.getRestartLSN(ctx, h.pgReplicationSlotName)
		if err != nil {
			return fmt.Errorf("get restart LSN: %w", err)
		}
	}

	h.logger.Trace("replication handler: set start LSN", logFields, loglib.Fields{
		logLSNPosition: pglogrepl.LSN(startPos),
	})

	pluginArguments := []string{
		`"include-timestamp" '1'`,
		`"format-version" '2'`,
		`"write-in-chunks" '1'`,
		`"include-lsn" '1'`,
		`"include-transaction" '0'`,
	}
	err = pglogrepl.StartReplication(
		ctx,
		h.pgReplicationConn,
		h.pgReplicationSlotName,
		pglogrepl.LSN(startPos),
		pglogrepl.StartReplicationOptions{PluginArgs: pluginArguments})
	if err != nil {
		return fmt.Errorf("startReplication: %w", err)
	}

	h.logger.Info("replication handler: logical replication started", logFields)

	return h.SyncLSN(ctx, startPos)
}

func (h *Handler) ReceiveMessage(ctx context.Context) (replication.Message, error) {
	msg, err := h.pgReplicationConn.ReceiveMessage(ctx)
	if err != nil {
		return nil, mapPostgresError(err)
	}

	switch msg := msg.(type) {
	case *pgproto3.CopyData:
		switch msg.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pka, err := pglogrepl.ParsePrimaryKeepaliveMessage(msg.Data[1:])
			if err != nil {
				return nil, fmt.Errorf("parse keep alive: %w", err)
			}
			pkaMessage := PrimaryKeepAliveMessage(pka)
			return &pkaMessage, nil
		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(msg.Data[1:])
			if err != nil {
				return nil, fmt.Errorf("parse xlog data: %w", err)
			}

			xldMessage := XLogDataMessage(xld)
			return &xldMessage, nil
		default:
			return nil, fmt.Errorf("%v: %w", msg.Data[0], ErrUnsupportedCopyDataMessage)
		}
	case *pgproto3.NoticeResponse:
		return nil, parseErrNoticeResponse(msg)
	default:
		// unexpected message (WAL error?)
		return nil, fmt.Errorf("unexpected message: %#v", msg)
	}
}

// SyncLSN notifies Postgres how far we have processed in the WAL.
func (h *Handler) SyncLSN(ctx context.Context, lsn replication.LSN) error {
	err := pglogrepl.SendStandbyStatusUpdate(
		ctx,
		h.pgReplicationConn,
		pglogrepl.StandbyStatusUpdate{WALWritePosition: pglogrepl.LSN(lsn)},
	)
	if err != nil {
		return fmt.Errorf("syncLSN: send status update: %w", err)
	}
	h.logger.Trace("stored new LSN position", loglib.Fields{
		logLSNPosition: pglogrepl.LSN(lsn),
	})
	return nil
}

func (h *Handler) GetLSNParser() replication.LSNParser {
	return h.lsnParser
}

// Close closes the database connections.
func (h *Handler) Close() error {
	err := h.pgReplicationConn.Close(context.Background())
	if err != nil {
		return err
	}
	return h.pgConn.Close(context.Background())
}

// getRestartLSN returns the absolute earliest possible LSN we can support. If
// the consumer's LSN is earlier than this, we cannot (easily) catch the
// consumer back up.
func (h *Handler) getRestartLSN(ctx context.Context, slotName string) (replication.LSN, error) {
	var restartLSN string
	err := h.pgConn.QueryRow(
		ctx,
		`select restart_lsn from pg_replication_slots where slot_name=$1`,
		slotName,
	).Scan(&restartLSN)
	if err != nil {
		// TODO: improve error message in case the slot doesn't exist
		return 0, err
	}
	return h.lsnParser.FromString(restartLSN)
}

// getLastSyncedLSN gets the `confirmed_flush_lsn` from PG. This is the last LSN
// that the consumer confirmed it had completed.
func (h *Handler) getLastSyncedLSN(ctx context.Context) (replication.LSN, error) {
	var confirmedFlushLSN string
	err := h.pgConn.QueryRow(ctx, `select confirmed_flush_lsn from pg_replication_slots where slot_name=$1`, h.pgReplicationSlotName).Scan(&confirmedFlushLSN)
	if err != nil {
		return 0, err
	}

	return h.lsnParser.FromString(confirmedFlushLSN)
}
