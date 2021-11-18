// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package pipeline

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/cdc/entry"
	"github.com/pingcap/ticdc/cdc/model"
	"github.com/pingcap/ticdc/cdc/processor/pipeline/system"
	"github.com/pingcap/ticdc/cdc/sorter"
	"github.com/pingcap/ticdc/cdc/sorter/memory"
	"github.com/pingcap/ticdc/cdc/sorter/unified"
	"github.com/pingcap/ticdc/pkg/actor"
	"github.com/pingcap/ticdc/pkg/actor/message"
	cdcContext "github.com/pingcap/ticdc/pkg/context"
	cerror "github.com/pingcap/ticdc/pkg/errors"
	"github.com/pingcap/ticdc/pkg/pipeline"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	flushMemoryMetricsDuration = time.Second * 5
)

type sorterNode struct {
	sorter sorter.EventSorter

	tableID   model.TableID
	tableName string // quoted schema and table, used in metrics only

	// for per-table flow control
	flowController tableFlowController

	mounter entry.Mounter

	eg     *errgroup.Group
	cancel context.CancelFunc

	// The latest resolved ts that sorter has received.
	resolvedTs model.Ts

	outputCh         chan pipeline.Message
	tableActorRouter *actor.Router
	isTableActorMode bool
	tableActorID     actor.ID
}

func newSorterNode(
	tableName string, tableID model.TableID, startTs model.Ts,
	flowController tableFlowController, mounter entry.Mounter,
) *sorterNode {
	return &sorterNode{
		tableName:      tableName,
		tableID:        tableID,
		flowController: flowController,
		mounter:        mounter,
		resolvedTs:     startTs,
		outputCh:       make(chan pipeline.Message, defaultOutputChannelSize),
	}
}

func (n *sorterNode) Init(ctx pipeline.NodeContext) error {
	wg := errgroup.Group{}
	return n.StartActorNode(ctx, nil, &wg, ctx.ChangefeedVars(), ctx.GlobalVars())
}

func (n *sorterNode) StartActorNode(ctx context.Context, tableActorRouter *actor.Router, wg *errgroup.Group, info *cdcContext.ChangefeedVars, vars *cdcContext.GlobalVars) error {
	n.eg = wg
	if tableActorRouter != nil {
		n.isTableActorMode = true
		n.tableActorRouter = tableActorRouter
		n.tableActorID = system.ActorID(info.ID, n.tableID)
	}
	stdCtx, cancel := context.WithCancel(ctx)
	n.cancel = cancel
	var sorter sorter.EventSorter
	sortEngine := info.Info.Engine
	switch sortEngine {
	case model.SortInMemory:
		sorter = memory.NewEntrySorter()
	case model.SortUnified, model.SortInFile /* `file` becomes an alias of `unified` for backward compatibility */ :
		if sortEngine == model.SortInFile {
			log.Warn("File sorter is obsolete and replaced by unified sorter. Please revise your changefeed settings",
				zap.String("changefeed-id", info.ID), zap.String("table-name", n.tableName))
		}
		sortDir := info.Info.SortDir
		err := unified.CheckDir(sortDir)
		if err != nil {
			return errors.Trace(err)
		}
		sorter, err = unified.NewUnifiedSorter(sortDir, info.ID, n.tableName, n.tableID, vars.CaptureInfo.AdvertiseAddr)
		if err != nil {
			return errors.Trace(err)
		}
	default:
		return cerror.ErrUnknownSortEngine.GenWithStackByArgs(sortEngine)
	}
	failpoint.Inject("ProcessorAddTableError", func() {
		failpoint.Return(errors.New("processor add table injected error"))
	})
	n.eg.Go(func() error {
		if !n.isTableActorMode {
			ctx.(pipeline.NodeContext).Throw(errors.Trace(sorter.Run(stdCtx)))
		} else {
			err := sorter.Run(stdCtx)
			if err != nil {
				log.Error("sorter stopped", zap.Error(err))
			}
			_ = n.tableActorRouter.SendB(stdCtx, n.tableActorID, message.StopMessage())
		}
		return nil
	})
	n.eg.Go(func() error {
		// Since the flowController is implemented by `Cond`, it is not cancelable
		// by a context. We need to listen on cancellation and aborts the flowController
		// manually.
		<-stdCtx.Done()
		n.flowController.Abort()
		return nil
	})
	n.eg.Go(func() error {
		lastSentResolvedTs := uint64(0)
		lastSendResolvedTsTime := time.Now() // the time at which we last sent a resolved-ts.
		lastCRTs := uint64(0)                // the commit-ts of the last row changed we sent.

		metricsTableMemoryHistogram := tableMemoryHistogram.WithLabelValues(info.ID, vars.CaptureInfo.AdvertiseAddr)
		metricsTicker := time.NewTicker(flushMemoryMetricsDuration)
		defer metricsTicker.Stop()

		for {
			select {
			case <-stdCtx.Done():
				return nil
			case <-metricsTicker.C:
				metricsTableMemoryHistogram.Observe(float64(n.flowController.GetConsumption()))
			case msg, ok := <-sorter.Output():
				if !ok {
					// sorter output channel closed
					return nil
				}
				if msg == nil || msg.RawKV == nil {
					log.Panic("unexpected empty msg", zap.Reflect("msg", msg))
				}
				if msg.RawKV.OpType != model.OpTypeResolved {
					size := uint64(msg.RawKV.ApproximateSize())
					commitTs := msg.CRTs
					// We interpolate a resolved-ts if none has been sent for some time.
					if time.Since(lastSendResolvedTsTime) > resolvedTsInterpolateInterval {
						// checks the condition: cur_event_commit_ts > prev_event_commit_ts > last_resolved_ts
						// If this is true, it implies that (1) the last transaction has finished, and we are processing
						// the first event in a new transaction, (2) a resolved-ts is safe to be sent, but it has not yet.
						// This means that we can interpolate prev_event_commit_ts as a resolved-ts, improving the frequency
						// at which the sink flushes.
						if lastCRTs > lastSentResolvedTs && commitTs > lastCRTs {
							lastSentResolvedTs = lastCRTs
							lastSendResolvedTsTime = time.Now()
							if n.isTableActorMode {
								_ = tableActorRouter.Send(n.tableActorID, message.BarrierMessage(lastCRTs))
							} else {
								ctx.(pipeline.NodeContext).SendToNextNode(pipeline.PolymorphicEventMessage(model.NewResolvedPolymorphicEvent(0, lastCRTs)))
							}
						}
					}
					// NOTE we allow the quota to be exceeded if blocking means interrupting a transaction.
					// Otherwise the pipeline would deadlock.
					err := n.flowController.Consume(commitTs, size, func() error {
						if lastCRTs > lastSentResolvedTs {
							// If we are blocking, we send a Resolved Event here to elicit a sink-flush.
							// Not sending a Resolved Event here will very likely deadlock the pipeline.
							lastSentResolvedTs = lastCRTs
							lastSendResolvedTsTime = time.Now()
							if n.isTableActorMode {
								_ = tableActorRouter.Send(n.tableActorID, message.BarrierMessage(lastCRTs))
							} else {
								ctx.(pipeline.NodeContext).SendToNextNode(pipeline.PolymorphicEventMessage(model.NewResolvedPolymorphicEvent(0, lastCRTs)))
							}
						}
						return nil
					})
					if err != nil {
						if cerror.ErrFlowControllerAborted.Equal(err) {
							log.Info("flow control cancelled for table",
								zap.Int64("tableID", n.tableID),
								zap.String("tableName", n.tableName))
						} else {
							if n.isTableActorMode {
								log.Error("sorter stopped", zap.Error(err))
								_ = tableActorRouter.SendB(stdCtx, n.tableActorID, message.StopMessage())
							} else {
								ctx.(pipeline.NodeContext).Throw(err)
							}
						}
						return nil
					}
					lastCRTs = commitTs

					// DESIGN NOTE: We send the messages to the mounter in this separate goroutine to prevent
					// blocking the whole pipeline.
					msg.SetUpFinishedChan()
					select {
					case <-ctx.Done():
						return nil
					case n.mounter.Input() <- msg:
					}
				} else {
					// handle OpTypeResolved
					if msg.CRTs < lastSentResolvedTs {
						continue
					}
					lastSentResolvedTs = msg.CRTs
					lastSendResolvedTsTime = time.Now()
				}

				if n.isTableActorMode {
					n.outputCh <- pipeline.PolymorphicEventMessage(msg)
					_ = tableActorRouter.Send(n.tableActorID, message.TickMessage())
				} else {
					ctx.(pipeline.NodeContext).SendToNextNode(pipeline.PolymorphicEventMessage(msg))
				}
			}
		}
	})
	n.sorter = sorter
	return nil
}

// Receive receives the message from the previous node
func (n *sorterNode) Receive(ctx pipeline.NodeContext) error {
	_, err := n.TryHandleDataMessage(ctx, ctx.Message())
	return err
}

func (n *sorterNode) TryHandleDataMessage(ctx context.Context, msg pipeline.Message) (bool, error) {
	switch msg.Tp {
	case pipeline.MessageTypePolymorphicEvent:
		rawKV := msg.PolymorphicEvent.RawKV
		if rawKV != nil && rawKV.OpType == model.OpTypeResolved {
			// Puller resolved ts should not fall back.
			resolvedTs := rawKV.CRTs
			oldResolvedTs := atomic.SwapUint64(&n.resolvedTs, resolvedTs)
			if oldResolvedTs > resolvedTs {
				log.Panic("resolved ts regression",
					zap.Int64("tableID", n.tableID),
					zap.Uint64("resolvedTs", resolvedTs),
					zap.Uint64("oldResolvedTs", oldResolvedTs))
			}
			atomic.StoreUint64(&n.resolvedTs, rawKV.CRTs)
		}
		if n.isTableActorMode {
			return n.sorter.TryAddEntry(ctx, msg.PolymorphicEvent)
		} else {
			n.sorter.AddEntry(ctx, msg.PolymorphicEvent)
			return true, nil
		}
	default:
		return trySendMessageToNextNode(ctx, n.isTableActorMode, n.outputCh, msg), nil
	}
}

func (n *sorterNode) Destroy(ctx pipeline.NodeContext) error {
	defer tableMemoryHistogram.DeleteLabelValues(ctx.ChangefeedVars().ID, ctx.GlobalVars().CaptureInfo.AdvertiseAddr)
	n.cancel()
	return n.eg.Wait()
}

func (n *sorterNode) ResolvedTs() model.Ts {
	return atomic.LoadUint64(&n.resolvedTs)
}

func (n *sorterNode) TryGetProcessedMessage() *pipeline.Message {
	return tryGetProcessedMessageFromChan(n.outputCh)
}
