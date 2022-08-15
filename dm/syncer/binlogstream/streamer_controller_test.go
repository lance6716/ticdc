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

package binlogstream

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"github.com/google/uuid"
	"github.com/pingcap/errors"
	"github.com/pingcap/tiflow/dm/pb"
	"github.com/pingcap/tiflow/dm/pkg/binlog"
	"github.com/pingcap/tiflow/dm/pkg/binlog/reader"
	tcontext "github.com/pingcap/tiflow/dm/pkg/context"
	"github.com/pingcap/tiflow/dm/pkg/log"
	"github.com/pingcap/tiflow/dm/relay"
	"github.com/stretchr/testify/require"
)

func TestToBinlogType(t *testing.T) {
	testCases := []struct {
		relay relay.Process
		tp    BinlogType
	}{
		{
			&relay.Relay{},
			LocalBinlog,
		}, {
			nil,
			RemoteBinlog,
		},
	}

	for _, testCase := range testCases {
		tp := RelayToBinlogType(testCase.relay)
		require.Equal(t, testCase.tp, tp)
	}
}

func TestCanErrorRetry(t *testing.T) {
	relay2 := &relay.Relay{}
	controller := NewStreamerController(
		replication.BinlogSyncerConfig{},
		true,
		nil,
		"",
		nil,
		relay2,
		log.L(),
	)

	mockErr := errors.New("test")

	// local binlog puller can always retry
	for i := 0; i < 5; i++ {
		require.True(t, controller.CanRetry(mockErr))
	}

	origCfg := minErrorRetryInterval
	minErrorRetryInterval = 100 * time.Millisecond
	defer func() {
		minErrorRetryInterval = origCfg
	}()

	// test with remote binlog
	controller = NewStreamerController(
		replication.BinlogSyncerConfig{},
		true,
		nil,
		"",
		nil,
		nil,
		log.L(),
	)

	require.True(t, controller.CanRetry(mockErr))
	require.False(t, controller.CanRetry(mockErr))
	time.Sleep(100 * time.Millisecond)
	require.True(t, controller.CanRetry(mockErr))
}

type mockStream struct {
	i      int
	events []*replication.BinlogEvent
}

func (m *mockStream) GetEvent(ctx context.Context) (*replication.BinlogEvent, error) {
	if m.i < len(m.events) {
		e := m.events[m.i]
		m.i++
		return e, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

type mockStreamProducer struct {
	stream *mockStream
}

func (m *mockStreamProducer) GenerateStreamFrom(location binlog.Location) (reader.Streamer, error) {
	return m.stream, nil
}

type expectedInfo struct {
	pos    uint32
	suffix int
	data   []byte
	op     pb.ErrorOp
}

func TestGetEventWithInject(t *testing.T) {
	upstream := &mockStream{
		events: []*replication.BinlogEvent{
			{
				Header: &replication.EventHeader{LogPos: 1010, EventSize: 10},
			},
			{
				Header:  &replication.EventHeader{LogPos: 1020, EventSize: 10},
				RawData: []byte("should inject before me at 1010"),
			},
		},
	}
	producer := &mockStreamProducer{upstream}

	controller := NewStreamerController4Test(producer, upstream)

	injectReq := &pb.HandleWorkerErrorRequest{
		Op:        pb.ErrorOp_Inject,
		BinlogPos: "(bin.000001, 1010)",
	}
	injectEvents := []*replication.BinlogEvent{
		{
			Header:  &replication.EventHeader{},
			RawData: []byte("inject 1"),
		},
		{
			Header:  &replication.EventHeader{},
			RawData: []byte("inject 2"),
		},
	}
	err := controller.Set(injectReq, injectEvents)
	require.NoError(t, err)
	loc := binlog.Location{Position: mysql.Position{
		Name: "bin.000001",
		Pos:  1000,
	}}
	controller.streamModifier.reset(loc)
	controller.upstream.locationRecorder.reset(loc)

	expecteds := []expectedInfo{
		{1010, 0, nil, pb.ErrorOp_InvalidErrorOp},
		{1010, 1, []byte("inject 1"), pb.ErrorOp_Inject},
		{1010, 2, []byte("inject 2"), pb.ErrorOp_Inject},
		{1020, 0, []byte("should inject before me at 1010"), pb.ErrorOp_InvalidErrorOp},
	}

	checkGetEvent(t, controller, expecteds)
}

func TestGetEventWithReplace(t *testing.T) {
	upstream := &mockStream{
		events: []*replication.BinlogEvent{
			{
				Header: &replication.EventHeader{LogPos: 1010, EventSize: 10},
			},
			{
				Header:  &replication.EventHeader{LogPos: 1020, EventSize: 10},
				RawData: []byte("should replace me at 1010"),
			},
		},
	}
	producer := &mockStreamProducer{upstream}

	controller := NewStreamerController4Test(producer, upstream)

	replaceReq := &pb.HandleWorkerErrorRequest{
		Op:        pb.ErrorOp_Replace,
		BinlogPos: "(bin.000001, 1010)",
	}
	replaceEvents := []*replication.BinlogEvent{
		{
			Header:  &replication.EventHeader{},
			RawData: []byte("replace 1"),
		},
		{
			Header:  &replication.EventHeader{},
			RawData: []byte("replace 2"),
		},
	}
	err := controller.Set(replaceReq, replaceEvents)
	require.NoError(t, err)
	loc := binlog.Location{Position: mysql.Position{
		Name: "bin.000001",
		Pos:  1000,
	}}
	controller.streamModifier.reset(loc)
	controller.upstream.locationRecorder.reset(loc)

	expecteds := []expectedInfo{
		{1010, 0, nil, pb.ErrorOp_InvalidErrorOp},
		{1010, 1, []byte("replace 1"), pb.ErrorOp_Replace},
		{1020, 0, []byte("replace 2"), pb.ErrorOp_Replace},
	}

	checkGetEvent(t, controller, expecteds)
}

func TestGetEventWithSkip(t *testing.T) {
	upstream := &mockStream{
		events: []*replication.BinlogEvent{
			{
				Header: &replication.EventHeader{LogPos: 1010, EventSize: 10},
			},
			{
				Header:  &replication.EventHeader{LogPos: 1020, EventSize: 10},
				RawData: []byte("should skip me at 1010"),
			},
		},
	}
	producer := &mockStreamProducer{upstream}

	controller := NewStreamerController4Test(producer, upstream)

	replaceReq := &pb.HandleWorkerErrorRequest{
		Op:        pb.ErrorOp_Skip,
		BinlogPos: "(bin.000001, 1010)",
	}
	err := controller.Set(replaceReq, nil)
	require.NoError(t, err)
	loc := binlog.Location{Position: mysql.Position{
		Name: "bin.000001",
		Pos:  1000,
	}}
	controller.streamModifier.reset(loc)
	controller.upstream.locationRecorder.reset(loc)

	expecteds := []expectedInfo{
		{1010, 0, nil, pb.ErrorOp_InvalidErrorOp},
		{1020, 0, []byte("should skip me at 1010"), pb.ErrorOp_Skip},
	}

	checkGetEvent(t, controller, expecteds)
}

func checkGetEvent(t *testing.T, controller *StreamerController, expecteds []expectedInfo) {
	t.Helper()

	var lastLoc binlog.Location

	ctx := tcontext.Background()
	for i, expected := range expecteds {
		event, op, err := controller.GetEvent(ctx)
		require.NoError(t, err)
		require.Equal(t, expected.pos, event.Header.LogPos)
		require.Equal(t, expected.op, op)

		curEndLoc := controller.GetCurEndLocation()
		require.Equal(t, expected.pos, curEndLoc.Position.Pos)
		require.Equal(t, expected.suffix, curEndLoc.Suffix)

		if i > 0 {
			curStartLoc := controller.GetCurStartLocation()
			require.Equal(t, lastLoc.Position.Pos, curStartLoc.Position.Pos)
			require.Equal(t, lastLoc.Suffix, curStartLoc.Suffix)
		}

		lastLoc = curEndLoc
	}
	ctx, cancel := ctx.WithTimeout(10 * time.Millisecond)
	defer cancel()
	// nolint:dogsled
	_, _, err := controller.GetEvent(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func (s *testLocationSuite) TestLocationsWithGTID() {
	events := s.generateDDLEvents()

	// we have 5 events, only last 2 of them
	s.Require().Len(events, 5)
	startLoc := binlog.Location{
		Position: mysql.Position{
			Name: s.binlogFile,
			Pos:  events[2].Header.LogPos,
		},
	}
	err := startLoc.SetGTID(s.prevGSet)
	s.Require().NoError(err)

	events = events[3:5]
	{
		s.Require().Equal(replication.GTID_EVENT, events[0].Header.EventType)
		s.Require().Equal(uint32(259), events[0].Header.LogPos)
		e := events[0].Event.(*replication.GTIDEvent)
		gtid := fmt.Sprintf("%s:%d", uuid.Must(uuid.FromBytes(e.SID)), e.GNO)
		s.Require().Equal("3ccc475b-2343-11e7-be21-6c0b84d59f30:15", gtid)
	}
	{
		s.Require().Equal(replication.QUERY_EVENT, events[1].Header.EventType)
		s.Require().Equal(uint32(322), events[1].Header.LogPos)
	}

	replaceReq := &pb.HandleWorkerErrorRequest{
		Op:        pb.ErrorOp_Replace,
		BinlogPos: "(mysql-bin.000001, 259)",
	}
	replaceEvents := []*replication.BinlogEvent{
		{
			Header:  &replication.EventHeader{},
			RawData: []byte("first replace event"),
		},
		{
			Header:  &replication.EventHeader{},
			RawData: []byte("seconds replace event"),
		},
	}

	// expected 3 events, check their start/end locations
	expectedLocations := make([]binlog.Location, 4)
	expectedLocations[0] = startLoc
	expectedLocations[1] = startLoc
	expectedLocations[1].Position.Pos = events[0].Header.LogPos
	expectedLocations[2] = expectedLocations[1]
	expectedLocations[2].Suffix = 1
	expectedLocations[3] = startLoc
	expectedLocations[3].Position.Pos = events[1].Header.LogPos
	err = expectedLocations[3].SetGTID(s.currGSet)
	s.Require().NoError(err)

	upstream := &mockStream{
		events: events,
	}
	producer := &mockStreamProducer{upstream}

	controller := NewStreamerController4Test(producer, upstream)

	err = controller.Set(replaceReq, replaceEvents)
	s.Require().NoError(err)

	controller.streamModifier.reset(startLoc)
	controller.upstream.locationRecorder.reset(startLoc)
	ctx := tcontext.Background()

	for i := 1; i < len(expectedLocations); i++ {
		s.T().Logf("#%d", i)
		// nolint:dogsled
		_, _, err = controller.GetEvent(ctx)
		s.Require().NoError(err)
		s.Require().Equal(expectedLocations[i-1].String(), controller.GetCurStartLocation().String())
		s.Require().Equal(expectedLocations[i].String(), controller.GetCurEndLocation().String())
	}

	ctx, cancel := ctx.WithTimeout(10 * time.Millisecond)
	defer cancel()
	// nolint:dogsled
	_, _, err = controller.GetEvent(ctx)
	s.Require().ErrorIs(err, context.DeadlineExceeded)
}
