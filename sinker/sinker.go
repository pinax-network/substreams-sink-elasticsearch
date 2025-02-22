package sinker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/streamingfast/bstream"
	"github.com/streamingfast/logging"
	"github.com/streamingfast/shutter"
	sink "github.com/streamingfast/substreams-sink"
	pbsubstreamsrpc "github.com/streamingfast/substreams/pb/sf/substreams/rpc/v2"
	pbsubstreams "github.com/streamingfast/substreams/pb/sf/substreams/v1"
	"github.com/yaroshkvorets/substreams-sink-elasticsearch/elastic"
	pbdatabase "github.com/yaroshkvorets/substreams-sink-elasticsearch/pb/substreams/sink/database/v1"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type ElasticSinker struct {
	*shutter.Shutter
	*sink.Sinker

	loader *elastic.Loader
	logger *zap.Logger
	tracer logging.Tracer

	stats      *Stats
	lastCursor *sink.Cursor
}

func New(sink *sink.Sinker, loader *elastic.Loader, logger *zap.Logger, tracer logging.Tracer) (*ElasticSinker, error) {
	s := &ElasticSinker{
		Shutter: shutter.New(),
		Sinker:  sink,

		loader: loader,
		logger: logger,
		tracer: tracer,

		stats: NewStats(logger),
	}

	s.OnTerminating(func(err error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.writeLastCursor(ctx, err)
	})

	return s, nil
}

func (s *ElasticSinker) writeLastCursor(ctx context.Context, err error) {
	if s.lastCursor == nil || err != nil {
		return
	}

	_ = s.loader.WriteCursor(ctx, s.OutputModuleHash(), s.lastCursor)
}

func (s *ElasticSinker) Run(ctx context.Context) {
	cursor, err := s.loader.GetCursor(ctx, s.OutputModuleHash())
	if err != nil && !errors.Is(err, elastic.ErrCursorNotFound) {
		s.Shutdown(fmt.Errorf("unable to retrieve cursor: %w", err))
		return
	}

	s.Sinker.OnTerminating(s.Shutdown)
	s.OnTerminating(func(err error) {
		s.stats.LogNow()
		s.logger.Info("elasticsearch sinker terminating", zap.Stringer("last_block_written", s.stats.lastBlock))
		s.Sinker.Shutdown(err)
	})

	s.OnTerminating(func(_ error) { s.stats.Close() })
	s.stats.OnTerminated(func(err error) { s.Shutdown(err) })

	logEach := 15 * time.Second
	if s.logger.Core().Enabled(zap.DebugLevel) {
		logEach = 5 * time.Second
	}

	s.stats.Start(logEach, cursor)

	s.logger.Info("starting elasticsearch sink", zap.Duration("stats_refresh_each", logEach), zap.Stringer("restarting_at", cursor.Block()))
	s.Sinker.Run(ctx, cursor, s)
}

func (s *ElasticSinker) HandleBlockScopedData(ctx context.Context, data *pbsubstreamsrpc.BlockScopedData, isLive *bool, cursor *sink.Cursor) error {
	output := data.Output

	if output.Name != s.OutputModuleName() {
		return fmt.Errorf("received data from wrong output module, expected to received from %q but got module's output for %q", s.OutputModuleName(), output.Name)
	}

	dbChanges := &pbdatabase.DatabaseChanges{}
	err := proto.Unmarshal(output.GetMapOutput().GetValue(), dbChanges)
	if err != nil {
		return fmt.Errorf("unmarshal database changes: %w", err)
	}

	err = s.applyDatabaseChanges(ctx, dataAsBlockRef(data), dbChanges)
	if err != nil {
		return fmt.Errorf("apply database changes: %w", err)
	}

	s.lastCursor = cursor

	return nil
}

func (s *ElasticSinker) HandleBlockUndoSignal(ctx context.Context, data *pbsubstreamsrpc.BlockUndoSignal, cursor *sink.Cursor) error {
	return fmt.Errorf("received undo signal but there is no handling of undo, this is because you used `--undo-buffer-size=0` which is invalid right now")
}

func (s *ElasticSinker) applyDatabaseChanges(ctx context.Context, block bstream.BlockRef, databaseChanges *pbdatabase.DatabaseChanges) (err error) {
	startTime := time.Now()
	defer func() {
		FlushDuration.AddInt64(time.Since(startTime).Nanoseconds())
	}()

	for _, change := range databaseChanges.TableChanges {
		id := change.Pk
		switch change.Operation {
		case pbdatabase.TableChange_UNSET:
		case pbdatabase.TableChange_CREATE:
			entity := map[string]interface{}{}

			for _, field := range change.Fields {
				var newValue interface{} = field.NewValue
				entity[field.Name] = newValue
			}
			err := s.loader.Save(ctx, change.Table, id, entity)
			if err != nil {
				return fmt.Errorf("saving entity %s with id %s: %w (Block %s)", change.Table, id, err, block)
			}
		case pbdatabase.TableChange_UPDATE:
			entityChanges := map[string]interface{}{}
			for _, field := range change.Fields {
				entityChanges[field.Name] = field.NewValue
			}
			err := s.loader.Update(ctx, change.Table, change.Pk, entityChanges)
			if err != nil {
				return fmt.Errorf("updating entity %s with id %s: %w (Block %s)", change.Table, id, err, block)
			}
		case pbdatabase.TableChange_DELETE:
			for range change.Fields {
				err := s.loader.Delete(ctx, change.Table, change.Pk)
				if err != nil {
					return fmt.Errorf("deleting entity %s with id %s: %w (Block %s)", change.Table, id, err, block)
				}
			}
		}
	}

	FlushCount.Inc()
	FlushedEntriesCount.AddInt(len(databaseChanges.TableChanges))
	s.stats.RecordBlock(block)

	return nil
}

func dataAsBlockRef(blockData *pbsubstreamsrpc.BlockScopedData) bstream.BlockRef {
	return clockAsBlockRef(blockData.Clock)
}

func clockAsBlockRef(clock *pbsubstreams.Clock) bstream.BlockRef {
	return bstream.NewBlockRef(clock.Id, clock.Number)
}
