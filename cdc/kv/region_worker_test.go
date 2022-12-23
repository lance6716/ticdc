// Copyright 2021 PingCAP, Inc.
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

package kv

import (
	"context"
	"math/rand"
	"runtime"
	"sync"
	"testing"

	"github.com/pingcap/kvproto/pkg/cdcpb"
	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/pkg/config"
	"github.com/pingcap/tiflow/pkg/spanz"
	"github.com/stretchr/testify/require"
	"github.com/tikv/client-go/v2/tikv"
)

func TestRegionStateManager(t *testing.T) {
	rsm := newRegionStateManager(4)

	regionID := uint64(1000)
	_, ok := rsm.getState(regionID)
	require.False(t, ok)

	rsm.setState(regionID, &regionFeedState{requestID: 2})
	state, ok := rsm.getState(regionID)
	require.True(t, ok)
	require.Equal(t, uint64(2), state.requestID)
}

func TestRegionStateManagerThreadSafe(t *testing.T) {
	rsm := newRegionStateManager(4)
	regionCount := 100
	regionIDs := make([]uint64, regionCount)
	for i := 0; i < regionCount; i++ {
		regionID := uint64(1000 + i)
		regionIDs[i] = regionID
		rsm.setState(regionID, &regionFeedState{requestID: uint64(i + 1), lastResolvedTs: uint64(1000)})
	}

	var wg sync.WaitGroup
	concurrency := 20
	wg.Add(concurrency * 2)
	for j := 0; j < concurrency; j++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 10000; i++ {
				idx := rand.Intn(regionCount)
				regionID := regionIDs[idx]
				s, ok := rsm.getState(regionID)
				require.True(t, ok)
				require.Equal(t, uint64(idx+1), s.requestID)
			}
		}()
	}
	for j := 0; j < concurrency; j++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 10000; i++ {
				// simulate write less than read
				if i%5 != 0 {
					continue
				}
				regionID := regionIDs[rand.Intn(regionCount)]
				s, ok := rsm.getState(regionID)
				require.True(t, ok)
				lastResolvedTs := s.getLastResolvedTs()
				s.updateResolvedTs(s.getLastResolvedTs() + 10)
				rsm.setState(regionID, s)
				require.GreaterOrEqual(t, s.getLastResolvedTs(), lastResolvedTs)
			}
		}()
	}
	wg.Wait()

	totalResolvedTs := uint64(0)
	for _, regionID := range regionIDs {
		s, ok := rsm.getState(regionID)
		require.True(t, ok)
		require.Greater(t, s.lastResolvedTs, uint64(1000))
		totalResolvedTs += s.lastResolvedTs
	}
}

func TestRegionStateManagerBucket(t *testing.T) {
	rsm := newRegionStateManager(-1)
	require.GreaterOrEqual(t, rsm.bucket, minRegionStateBucket)
	require.LessOrEqual(t, rsm.bucket, maxRegionStateBucket)

	bucket := rsm.bucket * 2
	rsm = newRegionStateManager(bucket)
	require.Equal(t, bucket, rsm.bucket)
}

func TestRegionWorkerPoolSize(t *testing.T) {
	conf := config.GetDefaultServerConfig()
	conf.KVClient.WorkerPoolSize = 0
	config.StoreGlobalServerConfig(conf)
	size := getWorkerPoolSize()
	min := func(a, b int) int {
		if a < b {
			return a
		}
		return b
	}
	require.Equal(t, min(runtime.NumCPU()*2, maxWorkerPoolSize), size)

	conf.KVClient.WorkerPoolSize = 5
	size = getWorkerPoolSize()
	require.Equal(t, 5, size)

	conf.KVClient.WorkerPoolSize = maxWorkerPoolSize + 1
	size = getWorkerPoolSize()
	require.Equal(t, maxWorkerPoolSize, size)
}

func TestRegionWokerHandleEventEntryEventOutOfOrder(t *testing.T) {
	// For UPDATE SQL, its prewrite event has both value and old value.
	// It is possible that TiDB prewrites multiple times for the same row when
	// there are other transactions it conflicts with. For this case,
	// if the value is not "short", only the first prewrite contains the value.
	//
	// TiKV may output events for the UPDATE SQL as following:
	//
	// TiDB: [Prwrite1]    [Prewrite2]      [Commit]
	//       v             v                v                                   Time
	// ---------------------------------------------------------------------------->
	//         ^            ^    ^           ^     ^       ^     ^          ^     ^
	// TiKV:   [Scan Start] [Send Prewrite2] [Send Commit] [Send Prewrite1] [Send Init]
	// TiCDC:                    [Recv Prewrite2]  [Recv Commit] [Recv Prewrite1] [Recv Init]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eventCh := make(chan model.RegionFeedEvent, 2)
	s := createFakeEventFeedSession(ctx)
	s.eventCh = eventCh
	state := newRegionFeedState(newSingleRegionInfo(
		tikv.RegionVerID{},
		spanz.ToSpan([]byte{}, spanz.UpperBoundKey),
		0, &tikv.RPCContext{}), 0)
	state.start()
	worker := newRegionWorker(model.ChangeFeedID{}, s, "")
	require.Equal(t, 2, cap(worker.outputCh))

	// Receive prewrite2 with empty value.
	events := &cdcpb.Event_Entries_{
		Entries: &cdcpb.Event_Entries{
			Entries: []*cdcpb.Event_Row{{
				StartTs:  1,
				Type:     cdcpb.Event_PREWRITE,
				OpType:   cdcpb.Event_Row_PUT,
				Key:      []byte("key"),
				Value:    nil,
				OldValue: []byte("oldvalue"),
			}},
		},
	}
	err := worker.handleEventEntry(ctx, events, state)
	require.Nil(t, err)

	// Receive commit.
	events = &cdcpb.Event_Entries_{
		Entries: &cdcpb.Event_Entries{
			Entries: []*cdcpb.Event_Row{{
				StartTs:  1,
				CommitTs: 2,
				Type:     cdcpb.Event_COMMIT,
				OpType:   cdcpb.Event_Row_PUT,
				Key:      []byte("key"),
			}},
		},
	}
	err = worker.handleEventEntry(context.Background(), events, state)
	require.Nil(t, err)

	// Must not output event.
	var event model.RegionFeedEvent
	var ok bool
	select {
	case event, ok = <-eventCh:
	default:
	}
	require.Falsef(t, ok, "%v", event)
	require.EqualValuesf(t, model.RegionFeedEvent{}, event, "%v", event)

	// Receive prewrite1 with actual value.
	events = &cdcpb.Event_Entries_{
		Entries: &cdcpb.Event_Entries{
			Entries: []*cdcpb.Event_Row{{
				StartTs:  1,
				Type:     cdcpb.Event_PREWRITE,
				OpType:   cdcpb.Event_Row_PUT,
				Key:      []byte("key"),
				Value:    []byte("value"),
				OldValue: []byte("oldvalue"),
			}},
		},
	}
	err = worker.handleEventEntry(ctx, events, state)
	require.Nil(t, err)

	// Must not output event.
	select {
	case event, ok = <-eventCh:
	default:
	}
	require.Falsef(t, ok, "%v", event)
	require.EqualValuesf(t, model.RegionFeedEvent{}, event, "%v", event)

	// Receive prewrite1 with actual value.
	events = &cdcpb.Event_Entries_{
		Entries: &cdcpb.Event_Entries{
			Entries: []*cdcpb.Event_Row{
				{
					Type: cdcpb.Event_INITIALIZED,
				},
			},
		},
	}
	err = worker.handleEventEntry(ctx, events, state)
	require.Nil(t, err)

	// Must output event.
	select {
	case event, ok = <-eventCh:
	default:
	}
	require.Truef(t, ok, "%v", event)
	require.EqualValuesf(t, model.RegionFeedEvent{
		RegionID: 0,
		Val: &model.RawKVEntry{
			OpType:   model.OpTypePut,
			Key:      []byte("key"),
			Value:    []byte("value"),
			StartTs:  1,
			CRTs:     2,
			RegionID: 0,
			OldValue: []byte("oldvalue"),
		},
	}, event, "%v", event)
}

func TestRegionWorkerHandleResolvedTs(t *testing.T) {
	ctx := context.Background()
	w := &regionWorker{}
	outputCh := make(chan model.RegionFeedEvent, 1)
	w.outputCh = outputCh
	w.rtsUpdateCh = make(chan *rtsUpdateEvent, 1)
	w.session = &eventFeedSession{client: &CDCClient{
		changefeed: model.DefaultChangeFeedID("1"),
	}}
	w.metrics = &regionWorkerMetrics{metricSendEventResolvedCounter: sendEventCounter.
		WithLabelValues("native-resolved", "n", "id")}
	s1 := newRegionFeedState(singleRegionInfo{
		verID: tikv.NewRegionVerID(1, 1, 1),
	}, 1)
	s1.initialized.Store(true)
	s1.lastResolvedTs = 9

	s2 := newRegionFeedState(singleRegionInfo{
		verID: tikv.NewRegionVerID(2, 2, 2),
	}, 2)
	s2.initialized.Store(true)
	s2.lastResolvedTs = 11

	s3 := newRegionFeedState(singleRegionInfo{
		verID: tikv.NewRegionVerID(3, 3, 3),
	}, 3)
	s3.initialized.Store(false)
	s3.lastResolvedTs = 8
	err := w.handleResolvedTs(ctx, &resolvedTsEvent{
		resolvedTs: 10,
		regions:    []*regionFeedState{s1, s2, s3},
	})
	require.Nil(t, err)
	require.Equal(t, uint64(10), s1.lastResolvedTs)
	require.Equal(t, uint64(11), s2.lastResolvedTs)
	require.Equal(t, uint64(8), s3.lastResolvedTs)

	re := <-w.rtsUpdateCh
	require.Equal(t, uint64(10), re.resolvedTs)
	require.Equal(t, 2, len(re.regions))

	event := <-outputCh
	require.Equal(t, uint64(0), event.RegionID)
	require.Equal(t, uint64(10), event.Resolved.ResolvedTs)
	require.Equal(t, 1, len(event.Resolved.Spans))
	require.Equal(t, uint64(1), event.Resolved.Spans[0].Region)
}
