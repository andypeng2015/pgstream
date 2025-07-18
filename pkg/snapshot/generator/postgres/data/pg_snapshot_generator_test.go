// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"
	"github.com/xataio/pgstream/internal/log/zerolog"
	pglib "github.com/xataio/pgstream/internal/postgres"
	pgmocks "github.com/xataio/pgstream/internal/postgres/mocks"
	"github.com/xataio/pgstream/internal/progress"
	progressmocks "github.com/xataio/pgstream/internal/progress/mocks"
	"github.com/xataio/pgstream/pkg/snapshot"
	"github.com/xataio/pgstream/pkg/snapshot/generator/mocks"
	"github.com/xataio/pgstream/pkg/wal"
)

func TestSnapshotGenerator_CreateSnapshot(t *testing.T) {
	t.Parallel()

	testTable1 := "test-table-1"
	testTable2 := "test-table-2"
	testSchema := "test-schema"
	testSnapshot := &snapshot.Snapshot{
		SchemaTables: map[string][]string{
			testSchema: {testTable1},
		},
	}
	quotedSchemaTable1 := pglib.QuoteQualifiedIdentifier(testSchema, testTable1)
	quotedSchemaTable2 := pglib.QuoteQualifiedIdentifier(testSchema, testTable2)

	txOptions := pglib.TxOptions{
		IsolationLevel: pglib.RepeatableRead,
		AccessMode:     pglib.ReadOnly,
	}

	testSnapshotID := "test-snapshot-id"
	testPageCount := 0 // 0 means 1 page
	testPageAvgBytes := int64(1024)
	testTotalBytes := int64(2048)
	testRowBytes := int64(512)
	testUUID := uuid.New().String()
	testUUID2 := uuid.New().String()
	testColumns := []snapshot.Column{
		{Name: "id", Type: "uuid", Value: testUUID},
		{Name: "name", Type: "text", Value: "alice"},
	}

	testRow := func(tableName string, columns []snapshot.Column) *snapshot.Row {
		return &snapshot.Row{
			Schema:  testSchema,
			Table:   tableName,
			Columns: columns,
		}
	}

	validTableInfoScanFn := func(args ...any) error {
		require.Len(t, args, 3)
		pageCount, ok := args[0].(*int)
		require.True(t, ok, fmt.Sprintf("pageCount, expected *int, got %T", args[0]))
		*pageCount = testPageCount
		pageAvgBytes, ok := args[1].(*int64)
		require.True(t, ok, fmt.Sprintf("pageAvgBytes, expected *int64, got %T", args[1]))
		*pageAvgBytes = testPageAvgBytes
		rowAvgBytes, ok := args[2].(*int64)
		require.True(t, ok, fmt.Sprintf("rowAvgBytes, expected *int64, got %T", args[2]))
		*rowAvgBytes = testRowBytes
		return nil
	}

	validMissedRowsScanFn := func(args ...any) error {
		require.Len(t, args, 1)
		rowCount, ok := args[0].(*int)
		require.True(t, ok, fmt.Sprintf("rowCount, expected *int, got %T", args[0]))
		*rowCount = 0
		return nil
	}

	validTableInfoQueryRowFn := func(ctx context.Context, query string, args ...any) pglib.Row {
		switch query {
		case tableInfoQuery:
			require.Equal(t, []any{testTable1, testSchema}, args)
			return &pgmocks.Row{
				ScanFn: validTableInfoScanFn,
			}
		case fmt.Sprintf(pageRangeQueryCount, quotedSchemaTable1, 1, 2):
			return &pgmocks.Row{
				ScanFn: validMissedRowsScanFn,
			}
		default:
			return &pgmocks.Row{
				ScanFn: func(args ...any) error {
					return fmt.Errorf("unexpected call to QueryRowFn: %s", query)
				},
			}
		}
	}

	errTest := errors.New("oh noes")

	tests := []struct {
		name          string
		querier       pglib.Querier
		snapshot      *snapshot.Snapshot
		schemaWorkers uint
		progressBar   *progressmocks.Bar

		wantRows []*snapshot.Row
		wantErr  error
	}{
		{
			name: "ok",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(_ context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 2:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryRowFn: validTableInfoQueryRowFn,
						}
						return f(&mockTx)
					case 3:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryFn: func(ctx context.Context, query string, args ...any) (pglib.Rows, error) {
								require.Equal(t, fmt.Sprintf(pageRangeQuery, quotedSchemaTable1, 0, 1), query)
								require.Len(t, args, 0)
								return &pgmocks.Rows{
									CloseFn: func() {},
									NextFn:  func(i uint) bool { return i == 1 },
									FieldDescriptionsFn: func() []pgconn.FieldDescription {
										return []pgconn.FieldDescription{
											{Name: "id", DataTypeOID: pgtype.UUIDOID},
											{Name: "name", DataTypeOID: pgtype.TextOID},
										}
									},
									ValuesFn: func() ([]any, error) {
										return []any{testUUID, "alice"}, nil
									},
									ErrFn: func() error { return nil },
								}, nil
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},

			wantErr:  nil,
			wantRows: []*snapshot.Row{testRow(testTable1, testColumns)},
		},
		{
			name: "ok - with missed pages",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(_ context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 2:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								switch query {
								case tableInfoQuery:
									require.Equal(t, []any{testTable1, testSchema}, args)
									return &pgmocks.Row{
										ScanFn: validTableInfoScanFn,
									}
								case fmt.Sprintf(pageRangeQueryCount, quotedSchemaTable1, 1, 2):
									return &pgmocks.Row{
										ScanFn: func(args ...any) error {
											require.Len(t, args, 1)
											rowCount, ok := args[0].(*int)
											require.True(t, ok, fmt.Sprintf("rowCount, expected *int, got %T", args[0]))
											*rowCount = 1
											return nil
										},
									}
								case fmt.Sprintf(pageRangeQueryCount, quotedSchemaTable1, 2, 3):
									return &pgmocks.Row{
										ScanFn: func(args ...any) error {
											require.Len(t, args, 1)
											rowCount, ok := args[0].(*int)
											require.True(t, ok, fmt.Sprintf("rowCount, expected *int, got %T", args[0]))
											*rowCount = 0
											return nil
										},
									}
								default:
									return &pgmocks.Row{
										ScanFn: func(args ...any) error {
											return fmt.Errorf("unexpected call to QueryRowFn: %s", query)
										},
									}
								}
							},
						}
						return f(&mockTx)
					case 3:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryFn: func(ctx context.Context, query string, args ...any) (pglib.Rows, error) {
								require.Equal(t, fmt.Sprintf(pageRangeQuery, quotedSchemaTable1, 0, 1), query)
								require.Len(t, args, 0)
								return &pgmocks.Rows{
									CloseFn: func() {},
									NextFn:  func(i uint) bool { return i == 1 },
									FieldDescriptionsFn: func() []pgconn.FieldDescription {
										return []pgconn.FieldDescription{
											{Name: "id", DataTypeOID: pgtype.UUIDOID},
											{Name: "name", DataTypeOID: pgtype.TextOID},
										}
									},
									ValuesFn: func() ([]any, error) {
										return []any{testUUID, "alice"}, nil
									},
									ErrFn: func() error { return nil },
								}, nil
							},
						}
						return f(&mockTx)
					case 4:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryFn: func(ctx context.Context, query string, args ...any) (pglib.Rows, error) {
								require.Equal(t, fmt.Sprintf(pageRangeQuery, quotedSchemaTable1, 1, 2), query)
								require.Len(t, args, 0)
								return &pgmocks.Rows{
									CloseFn: func() {},
									NextFn:  func(i uint) bool { return i == 1 },
									FieldDescriptionsFn: func() []pgconn.FieldDescription {
										return []pgconn.FieldDescription{
											{Name: "id", DataTypeOID: pgtype.UUIDOID},
											{Name: "name", DataTypeOID: pgtype.TextOID},
										}
									},
									ValuesFn: func() ([]any, error) {
										return []any{testUUID2, "bob"}, nil
									},
									ErrFn: func() error { return nil },
								}, nil
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},

			wantErr: nil,
			wantRows: []*snapshot.Row{
				testRow(testTable1, testColumns),
				testRow(testTable1, []snapshot.Column{
					{Name: "id", Type: "uuid", Value: testUUID2},
					{Name: "name", Type: "text", Value: "bob"},
				}),
			},
		},
		{
			name: "ok - with progress tracking",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(_ context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 2:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, fmt.Sprintf(tablesBytesQuery, testSchema, "$1"), query)
								require.Equal(t, []any{testTable1}, args)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										totalBytes, ok := args[0].(*int64)
										require.True(t, ok, fmt.Sprintf("totalBytes, expected *int64, got %T", args[0]))
										*totalBytes = testTotalBytes
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 3:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryRowFn: validTableInfoQueryRowFn,
						}
						return f(&mockTx)
					case 4:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryFn: func(ctx context.Context, query string, args ...any) (pglib.Rows, error) {
								require.Equal(t, fmt.Sprintf(pageRangeQuery, quotedSchemaTable1, 0, 1), query)
								require.Len(t, args, 0)
								return &pgmocks.Rows{
									CloseFn: func() {},
									NextFn:  func(i uint) bool { return i == 1 },
									FieldDescriptionsFn: func() []pgconn.FieldDescription {
										return []pgconn.FieldDescription{
											{Name: "id", DataTypeOID: pgtype.UUIDOID},
											{Name: "name", DataTypeOID: pgtype.TextOID},
										}
									},
									ValuesFn: func() ([]any, error) {
										return []any{testUUID, "alice"}, nil
									},
									ErrFn: func() error { return nil },
								}, nil
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},
			progressBar: &progressmocks.Bar{
				Add64Fn: func(n int64) error {
					require.Equal(t, testRowBytes, n) // only 1 row processed
					return nil
				},
			},

			wantErr:  nil,
			wantRows: []*snapshot.Row{testRow(testTable1, testColumns)},
		},
		{
			name: "ok - multiple tables and multiple workers",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(_ context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					mockTx := pgmocks.Tx{
						QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
							if query == exportSnapshotQuery {
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							}
							if query == tableInfoQuery {
								return &pgmocks.Row{
									ScanFn: validTableInfoScanFn,
								}
							}
							if query == fmt.Sprintf(pageRangeQueryCount, quotedSchemaTable1, 1, 2) ||
								query == fmt.Sprintf(pageRangeQueryCount, quotedSchemaTable2, 1, 2) {
								return &pgmocks.Row{
									ScanFn: validMissedRowsScanFn,
								}
							}
							return &pgmocks.Row{
								ScanFn: func(args ...any) error { return fmt.Errorf("unexpected call to QueryRowFn: %s", query) },
							}
						},
						QueryFn: func(ctx context.Context, query string, args ...any) (pglib.Rows, error) {
							return &pgmocks.Rows{
								CloseFn: func() {},
								NextFn:  func(i uint) bool { return i == 1 },
								FieldDescriptionsFn: func() []pgconn.FieldDescription {
									return []pgconn.FieldDescription{
										{Name: "id", DataTypeOID: pgtype.UUIDOID},
										{Name: "name", DataTypeOID: pgtype.TextOID},
									}
								},
								ValuesFn: func() ([]any, error) {
									return []any{testUUID, "alice"}, nil
								},
								ErrFn: func() error { return nil },
							}, nil
						},
						ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
							require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
							require.Len(t, args, 0)
							return pglib.CommandTag{}, nil
						},
					}
					return f(&mockTx)
				},
			},
			snapshot: &snapshot.Snapshot{
				SchemaTables: map[string][]string{
					testSchema: {testTable1, testTable2},
				},
			},
			schemaWorkers: 2,

			wantErr:  nil,
			wantRows: []*snapshot.Row{testRow(testTable1, testColumns), testRow(testTable2, testColumns)},
		},
		{
			name: "ok - unsupported column type",
			querier: &pgmocks.Querier{
				QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
					return &pgmocks.Row{
						ScanFn: func(args ...any) error { return errTest },
					}
				},
				ExecInTxWithOptionsFn: func(_ context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 2:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryRowFn: validTableInfoQueryRowFn,
						}
						return f(&mockTx)
					case 3:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryFn: func(ctx context.Context, query string, args ...any) (pglib.Rows, error) {
								require.Equal(t, fmt.Sprintf(pageRangeQuery, quotedSchemaTable1, 0, 1), query)
								require.Len(t, args, 0)
								return &pgmocks.Rows{
									CloseFn: func() {},
									NextFn:  func(i uint) bool { return i == 1 },
									FieldDescriptionsFn: func() []pgconn.FieldDescription {
										return []pgconn.FieldDescription{
											{Name: "id", DataTypeOID: pgtype.UUIDOID},
											{Name: "name", DataTypeOID: pgtype.TextOID},
											{Name: "unsupported", DataTypeOID: 99999},
										}
									},
									ValuesFn: func() ([]any, error) {
										return []any{testUUID, "alice", 1}, nil
									},
									ErrFn: func() error { return nil },
								}, nil
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},

			wantErr:  nil,
			wantRows: []*snapshot.Row{testRow(testTable1, testColumns)},
		},
		{
			name: "ok - no data",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(_ context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 2:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryRowFn: validTableInfoQueryRowFn,
						}
						return f(&mockTx)
					case 3:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryFn: func(ctx context.Context, query string, args ...any) (pglib.Rows, error) {
								require.Equal(t, fmt.Sprintf(pageRangeQuery, quotedSchemaTable1, 0, 1), query)
								require.Len(t, args, 0)
								return &pgmocks.Rows{
									CloseFn:             func() {},
									NextFn:              func(i uint) bool { return i == 0 },
									FieldDescriptionsFn: func() []pgconn.FieldDescription { return []pgconn.FieldDescription{} },
									ValuesFn:            func() ([]any, error) { return []any{}, nil },
									ErrFn:               func() error { return nil },
								}, nil
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},

			wantErr:  nil,
			wantRows: []*snapshot.Row{},
		},
		{
			name: "error - exporting snapshot",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(_ context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										return errTest
									},
								}
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},

			wantErr:  snapshot.NewErrors(testSchema, fmt.Errorf("exporting snapshot: %w", errTest)),
			wantRows: []*snapshot.Row{},
		},
		{
			name: "error - setting transaction snapshot before table page count",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(ctx context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 2:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, errTest
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},

			wantErr: snapshot.Errors{
				testSchema: &snapshot.SchemaErrors{
					Schema: testSchema,
					TableErrors: map[string]string{
						testTable1: fmt.Sprintf("setting transaction snapshot: %v", errTest),
					},
				},
			},
			wantRows: []*snapshot.Row{},
		},
		{
			name: "error - getting table page count",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(ctx context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 2:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, tableInfoQuery, query)
								require.Equal(t, []any{testTable1, testSchema}, args)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										return errTest
									},
								}
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},

			wantErr: snapshot.Errors{
				testSchema: &snapshot.SchemaErrors{
					Schema: testSchema,
					TableErrors: map[string]string{
						testTable1: fmt.Sprintf("getting page information for table test-schema.test-table-1: %v", errTest),
					},
				},
			},
			wantRows: []*snapshot.Row{},
		},
		{
			name: "error - setting transaction snapshot for table range",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(ctx context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 2:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryRowFn: validTableInfoQueryRowFn,
						}
						return f(&mockTx)
					case 3:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, errTest
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},

			wantErr: snapshot.Errors{
				testSchema: &snapshot.SchemaErrors{
					Schema: testSchema,
					TableErrors: map[string]string{
						testTable1: fmt.Sprintf("setting transaction snapshot: %v", errTest),
					},
				},
			},
			wantRows: []*snapshot.Row{},
		},
		{
			name: "error - querying range data",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(ctx context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 2:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryRowFn: validTableInfoQueryRowFn,
						}
						return f(&mockTx)
					case 3:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryFn: func(ctx context.Context, query string, args ...any) (pglib.Rows, error) {
								require.Equal(t, fmt.Sprintf(pageRangeQuery, quotedSchemaTable1, 0, 1), query)
								require.Len(t, args, 0)
								return nil, errTest
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},

			wantErr: snapshot.Errors{
				testSchema: &snapshot.SchemaErrors{
					Schema: testSchema,
					TableErrors: map[string]string{
						testTable1: fmt.Sprintf("querying table rows: %v", errTest),
					},
				},
			},
			wantRows: []*snapshot.Row{},
		},
		{
			name: "error - getting row values",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(ctx context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 2:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryRowFn: validTableInfoQueryRowFn,
						}
						return f(&mockTx)
					case 3:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryFn: func(ctx context.Context, query string, args ...any) (pglib.Rows, error) {
								return &pgmocks.Rows{
									CloseFn:             func() {},
									NextFn:              func(i uint) bool { return i == 1 },
									ValuesFn:            func() ([]any, error) { return nil, errTest },
									FieldDescriptionsFn: func() []pgconn.FieldDescription { return []pgconn.FieldDescription{} },
									ErrFn:               func() error { return nil },
								}, nil
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},

			wantErr: snapshot.Errors{
				testSchema: &snapshot.SchemaErrors{
					Schema: testSchema,
					TableErrors: map[string]string{
						testTable1: fmt.Sprintf("retrieving rows values: %v", errTest),
					},
				},
			},
			wantRows: []*snapshot.Row{},
		},
		{
			name: "error - rows err",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(ctx context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 2:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryRowFn: validTableInfoQueryRowFn,
						}
						return f(&mockTx)
					case 3:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryFn: func(ctx context.Context, query string, args ...any) (pglib.Rows, error) {
								return &pgmocks.Rows{
									CloseFn:             func() {},
									NextFn:              func(i uint) bool { return i == 1 },
									ValuesFn:            func() ([]any, error) { return []any{}, nil },
									FieldDescriptionsFn: func() []pgconn.FieldDescription { return []pgconn.FieldDescription{} },
									ErrFn:               func() error { return errTest },
								}, nil
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},

			wantErr: snapshot.Errors{
				testSchema: &snapshot.SchemaErrors{
					Schema: testSchema,
					TableErrors: map[string]string{
						testTable1: errTest.Error(),
					},
				},
			},
			wantRows: []*snapshot.Row{},
		},
		{
			name: "error - multiple tables and multiple workers",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(_ context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					mockTx := pgmocks.Tx{
						QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
							if query == exportSnapshotQuery {
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							}
							if query == tableInfoQuery {
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										return errTest
									},
								}
							}
							return &pgmocks.Row{
								ScanFn: func(args ...any) error { return fmt.Errorf("unexpected call to QueryRowFn: %s", query) },
							}
						},
						ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
							require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
							require.Len(t, args, 0)
							return pglib.CommandTag{}, nil
						},
					}
					return f(&mockTx)
				},
			},
			snapshot: &snapshot.Snapshot{
				SchemaTables: map[string][]string{
					testSchema: {testTable1, testTable2},
				},
			},
			schemaWorkers: 2,

			wantErr: snapshot.Errors{
				testSchema: &snapshot.SchemaErrors{
					Schema: testSchema,
					TableErrors: map[string]string{
						testTable1: fmt.Sprintf("getting page information for table %s.%s: %v", testSchema, testTable1, errTest),
						testTable2: fmt.Sprintf("getting page information for table %s.%s: %v", testSchema, testTable2, errTest),
					},
				},
			},
			wantRows: []*snapshot.Row{},
		},
		{
			name: "error - adding progress bar",
			querier: &pgmocks.Querier{
				ExecInTxWithOptionsFn: func(_ context.Context, i uint, f func(tx pglib.Tx) error, to pglib.TxOptions) error {
					require.Equal(t, txOptions, to)
					switch i {
					case 1:
						mockTx := pgmocks.Tx{
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, exportSnapshotQuery, query)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										require.Len(t, args, 1)
										snapshotID, ok := args[0].(*string)
										require.True(t, ok, fmt.Sprintf("snapshotID, expected *string, got %T", args[0]))
										*snapshotID = testSnapshotID
										return nil
									},
								}
							},
						}
						return f(&mockTx)
					case 2:
						mockTx := pgmocks.Tx{
							ExecFn: func(ctx context.Context, _ uint, query string, args ...any) (pglib.CommandTag, error) {
								require.Equal(t, fmt.Sprintf("SET TRANSACTION SNAPSHOT '%s'", testSnapshotID), query)
								require.Len(t, args, 0)
								return pglib.CommandTag{}, nil
							},
							QueryRowFn: func(ctx context.Context, query string, args ...any) pglib.Row {
								require.Equal(t, fmt.Sprintf(tablesBytesQuery, testSchema, "$1"), query)
								require.Equal(t, []any{testTable1}, args)
								return &pgmocks.Row{
									ScanFn: func(args ...any) error {
										return errTest
									},
								}
							},
						}
						return f(&mockTx)
					default:
						return fmt.Errorf("unexpected call to ExecInTxWithOptions: %d", i)
					}
				},
			},
			progressBar: &progressmocks.Bar{
				Add64Fn: func(n int64) error {
					return errors.New("Add64Fn should not be called")
				},
			},

			wantErr: snapshot.Errors{
				testSchema: &snapshot.SchemaErrors{
					Schema:       testSchema,
					GlobalErrors: []string{fmt.Sprintf("retrieving total bytes for schema: %v", errTest)},
				},
			},
			wantRows: []*snapshot.Row{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rowChan := make(chan *snapshot.Row, 10)
			sg := SnapshotGenerator{
				logger: zerolog.NewStdLogger(zerolog.NewLogger(&zerolog.Config{
					LogLevel: "debug",
				})),
				conn:   tc.querier,
				mapper: pglib.NewMapper(tc.querier),
				rowsProcessor: &mocks.RowsProcessor{
					ProcessRowFn: func(ctx context.Context, e *snapshot.Row) error {
						rowChan <- e
						return nil
					},
				},
				schemaWorkers:    1,
				tableWorkers:     1,
				batchBytes:       1024 * 1024, // 1MB
				snapshotWorkers:  1,
				progressTracking: tc.progressBar != nil,
				progressBars:     make(map[string]progress.Bar),
				progressBarBuilder: func(totalBytes int64, description string) progress.Bar {
					return tc.progressBar
				},
			}
			sg.tableSnapshotGenerator = sg.snapshotTable

			if tc.schemaWorkers != 0 {
				sg.schemaWorkers = tc.schemaWorkers
			}

			s := testSnapshot
			if tc.snapshot != nil {
				s = tc.snapshot
			}

			err := sg.CreateSnapshot(context.Background(), s)
			require.Equal(t, tc.wantErr, err)
			close(rowChan)

			rows := []*snapshot.Row{}
			for row := range rowChan {
				rows = append(rows, row)
			}
			diff := cmp.Diff(rows, tc.wantRows,
				cmpopts.IgnoreFields(wal.Data{}, "Timestamp"),
				cmpopts.SortSlices(func(a, b *snapshot.Row) bool { return a.Table < b.Table }))
			require.Empty(t, diff, fmt.Sprintf("got: \n%v, \nwant \n%v, \ndiff: \n%s", rows, tc.wantRows, diff))
		})
	}
}

func TestTablePageInfo_calculateBatchPageSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tableInfo *tableInfo
		bytes     uint64

		wantPageSize uint
	}{
		{
			name: "no avg page bytes - return total pages",
			tableInfo: &tableInfo{
				pageCount:    1,
				avgPageBytes: 0,
			},
			wantPageSize: 1,
		},
		{
			name: "single page - return total pages",
			tableInfo: &tableInfo{
				pageCount:    0,
				avgPageBytes: 0,
			},
			wantPageSize: 1,
		},
		{
			name: "multiple pages with average page bytes",
			tableInfo: &tableInfo{
				pageCount:    100,
				avgPageBytes: 10,
			},
			bytes:        1000,
			wantPageSize: 100,
		},
		{
			name: "page size != page count",
			tableInfo: &tableInfo{
				pageCount:    100,
				avgPageBytes: 10,
			},
			bytes:        10,
			wantPageSize: 1,
		},
		{
			name: "bytes > avgPageBytes",
			tableInfo: &tableInfo{
				pageCount:    100,
				avgPageBytes: 11,
			},
			bytes:        10,
			wantPageSize: 1,
		},
		{
			name: "batch page size > total pages",
			tableInfo: &tableInfo{
				pageCount:    1,
				avgPageBytes: 10,
			},
			bytes:        1000,
			wantPageSize: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tc.tableInfo.calculateBatchPageSize(tc.bytes)
			require.Equal(t, tc.wantPageSize, tc.tableInfo.batchPageSize, "wanted page size %d, got %d", tc.wantPageSize, tc.tableInfo.batchPageSize)
		})
	}
}
